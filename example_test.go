package mangrove_test

import (
	"context"
	"fmt"
	"time"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
	"github.com/ourstudio-se/mangrove-go/memorycache"
)

// The README walkthrough: entities are refreshed one by one, and new list
// members are fetched through links.
func ExampleMangrove_Wrap() {
	ctx := context.Background()
	items := []obj{{"id": "1"}, {"id": "2"}}
	generation := 0
	var resolved []string

	schema := testexec.MustSchema(`
		directive @cacheResolver(keyArg: String) on FIELD_DEFINITION
		type Query { items: [Item!]! item(id: ID!): Item @cacheResolver }
		type Item { id: ID! data: String! }`,
		testexec.Resolvers{
			"Item": {"data": fn(func(item obj, _ obj) any {
				resolved = append(resolved, item["id"].(string))
				return fmt.Sprintf("%v-%d", item["id"], generation)
			})},
			"Query": {
				"item":  fn(func(_ obj, args obj) any { return findByID(items, args["id"]) }),
				"items": fn0(func() any { return list(items...) }),
			},
		})

	strategy := mangrove.NewLazyStrategy(mangrove.LazyStrategyOptions{Cache: memorycache.New(memorycache.Options{})})
	m, err := mangrove.New(mangrove.Options{
		Strategy:                 strategy,
		TTL:                      time.Minute,
		Schema:                   schema.Schema,
		AwaitWriteBeforeResponse: true,
	})
	if err != nil {
		panic(err)
	}
	execute := m.Wrap(schema.Executor())
	document := schema.MustParse(`query BigItemQuery { items { id data } }`)

	run := func() {
		resolved = nil
		response, err := execute(ctx, &mangrove.Request{Document: document})
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s resolved %v\n", response.Data, resolved)
	}

	run()
	generation = 1
	run()

	_ = strategy.InvalidateEntities(ctx, []mangrove.EntityRecord{mangrove.Entity("Item", "1")})
	run()

	items = append(items, obj{"id": "3"})
	_ = strategy.InvalidateEntities(ctx, []mangrove.EntityRecord{mangrove.EntityType("Query")})
	run()

	// Output:
	// {"items":[{"id":"1","data":"1-0"},{"id":"2","data":"2-0"}]} resolved [1 2]
	// {"items":[{"id":"1","data":"1-0"},{"id":"2","data":"2-0"}]} resolved []
	// {"items":[{"id":"1","data":"1-1"},{"id":"2","data":"2-0"}]} resolved [1]
	// {"items":[{"id":"1","data":"1-1"},{"id":"2","data":"2-0"},{"id":"3","data":"3-1"}]} resolved [3]
}
