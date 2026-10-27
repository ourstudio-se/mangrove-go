package mangrove_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
	"github.com/ourstudio-se/mangrove-go/rediscache"
)

// The scenarios of the TS library's scripts/bench.mjs, against Redis:
//
//	nice go test -run '^$' -bench . -benchtime 100x
const (
	benchItems  = 200
	benchOwners = 50
)

type benchData struct {
	items  []obj
	owners map[string]obj
}

func newBenchData() *benchData {
	d := &benchData{owners: map[string]obj{}}
	for i := 0; i < benchOwners; i++ {
		id := fmt.Sprintf("o%d", i)
		d.owners[id] = obj{"id": id, "name": fmt.Sprintf("owner %d", i)}
	}
	for i := 0; i < benchItems; i++ {
		d.items = append(d.items, obj{
			"id":    fmt.Sprint(i),
			"name":  fmt.Sprintf("item %d", i),
			"owner": fmt.Sprintf("o%d", i%benchOwners),
			"tags":  []string{fmt.Sprintf("t%d", i%7), fmt.Sprintf("t%d", (i+1)%7), fmt.Sprintf("t%d", (i+2)%7)},
		})
	}
	return d
}

func (d *benchData) resolvers() testexec.Resolvers {
	return testexec.Resolvers{
		"Item": {
			"owner": fn(func(item obj, _ obj) any { return d.owners[item["owner"].(string)] }),
			"tags": fn(func(item obj, _ obj) any {
				var out []any
				for _, id := range item["tags"].([]string) {
					out = append(out, obj{"id": id, "label": id})
				}
				return out
			}),
		},
		"Query": {
			"item":  fn(func(_ obj, args obj) any { return findByID(d.items, args["id"]) }),
			"items": fn0(func() any { return list(d.items...) }),
			"owner": fn(func(_ obj, args obj) any { return d.owners[args["id"].(string)] }),
		},
	}
}

const benchSchema = `
type Query {
  items: [Item!]!
  item(id: ID!): Item @cacheResolver
  owner(id: ID!): Owner @cacheResolver
}
type Item { id: ID! name: String! owner: Owner! tags: [Tag!]! }
type Owner { id: ID! name: String! }
type Tag { id: ID! label: String! }
`

const benchQuery = `query Bench { items { id name owner { id name } tags { id label } } }`

func BenchmarkScenarios(b *testing.B) {
	ctx := context.Background()
	client := newRedisClient(b)
	redisMu.Lock()
	defer redisMu.Unlock()

	d := newBenchData()
	schema := testexec.MustSchema(directives+benchSchema, d.resolvers())
	strategy := mangrove.NewLazyStrategy(mangrove.LazyStrategyOptions{Cache: rediscache.New(rediscache.Options{Client: client})})
	m, err := mangrove.New(mangrove.Options{
		Strategy:                 strategy,
		TTL:                      time.Hour,
		Schema:                   schema.Schema,
		AwaitWriteBeforeResponse: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	// Like a server, the executor parses and validates every document.
	execute := m.Wrap(schema.Executor())
	document := schema.MustParse(benchQuery)
	run := func() *mangrove.Response {
		response, err := execute(ctx, &mangrove.Request{Document: document})
		if err != nil {
			b.Fatal(err)
		}
		return response
	}
	invalidate := func(entities ...mangrove.EntityRecord) {
		if err := strategy.InvalidateEntities(ctx, entities); err != nil {
			b.Fatal(err)
		}
	}

	b.Run("plain execution (no Mangrove)", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := schema.Execute(ctx, document, "", nil); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("miss", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			if err := client.FlushAll(ctx).Err(); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			run()
		}
	})

	run()
	b.Run("hit", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			run()
		}
	})

	b.Run("partial: one item renamed", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			item := d.items[i%benchItems]
			item["name"] = fmt.Sprintf("renamed %d", i)
			invalidate(mangrove.Entity("Item", item["id"].(string)))
			b.StartTimer()
			run()
		}
	})

	b.Run("partial: one owner renamed (on 4 items)", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			id := fmt.Sprintf("o%d", i%benchOwners)
			d.owners[id]["name"] = fmt.Sprintf("renamed %d", i)
			invalidate(mangrove.Entity("Owner", id))
			b.StartTimer()
			run()
		}
	})

	b.Run("partial: list reordered (Query invalidated)", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			d.items = append(d.items[1:], d.items[0])
			invalidate(mangrove.EntityType("Query"))
			b.StartTimer()
			run()
		}
	})

	// Sanity check: the cached result must equal direct execution.
	cached := run()
	direct, err := schema.Execute(ctx, schema.MustParse(benchQuery), "", nil)
	if err != nil {
		b.Fatal(err)
	}
	if string(cached.Data) != string(direct.Data) {
		b.Fatal("the cached result differs from direct execution")
	}
}
