package mangrove_test

import (
	"context"
	"fmt"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
)

// Findings of an independent review of the TS rework, as regression tests.
func TestReview(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		t.Run("R1: an older concurrent store doesn't overwrite a newer result", func(t *testing.T) {
			item := obj{"data": "v1", "id": "1"}
			g := newGate()
			var calls atomic.Int64
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Item": {"data": fnCtx(func(ctx context.Context, parent obj, _ obj) (any, error) {
						value := parent["data"]
						if !isDirect(ctx) && calls.Add(1) == 1 {
							g.wait()
						}
						return value, nil
					})},
					"Query": {"item": fn0(func() any { return copyObj(item) })},
				},
				typeDefs: `type Query { item: Item! } type Item { id: ID! data: String! }`,
			})
			query := `{ item { id data } }`

			a := h.runAsync(query) // reads v1, then stalls
			<-g.started
			item["data"] = "v2"
			h.invalidate(mangrove.Entity("Item", "1"))

			// Started after the invalidation.
			expectEqual(t, h.run(query).path(t, "item", "data"), "v2")

			g.open()
			a.wait(t) // stores its (older) v1 result

			// Started after the invalidation completed.
			expectEqual(t, h.run(query).path(t, "item", "data"), "v2")
		})

		t.Run("R2: a non-entity union member next to linkable entities isn't kept stale", func(t *testing.T) {
			bName := "old"
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {
					"a": fn(func(_ obj, args obj) any {
						return obj{"__typename": "A", "id": args["id"], "name": fmt.Sprint("a", args["id"])}
					}),
					"things": fn0(func() any {
						return list(obj{"__typename": "A", "id": "1", "name": "a1"}, obj{"__typename": "B", "name": bName})
					}),
				}},
				typeDefs: `
					type Query { things: [Thing!]! a(id: ID!): A @cacheResolver }
					union Thing = A | B
					type A { id: ID! name: String! }
					type B { name: String! }`,
			})
			query := `{ things { ... on A { id name } ... on B { name } } }`
			h.run(query)
			bName = "new"
			// B has no identity; its parent (Query) owns it.
			h.invalidate(mangrove.EntityType("Query"))
			expectEqual(t, h.run(query).path(t, "things", 1, "name"), "new")
		})

		t.Run("R3: union members whose id fields differ in nullability", func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {"things": fn0(func() any {
					return list(obj{"__typename": "A", "id": "1", "name": "a"}, obj{"__typename": "B", "id": "2", "name": "b"})
				})}},
				typeDefs: `
					type Query { things: [Thing!]! }
					union Thing = A | B
					type A { id: ID! name: String! }
					type B { id: ID name: String! }`,
			})
			expectData(t, h.run(`{ things { ... on A { name } ... on B { name } } }`), `{"things":[{"name":"a"},{"name":"b"}]}`)
		})

		t.Run("R4: interface id nullable, implementation id non-null", func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {"nodes": fn0(func() any {
					return list(obj{"__typename": "A", "id": "1", "name": "a"})
				})}},
				typeDefs: `
					type Query { nodes: [Node!]! }
					interface Node { id: ID }
					type A implements Node { id: ID! name: String! }`,
			})
			expectData(t, h.run(`{ nodes { ... on A { name } } }`), `{"nodes":[{"name":"a"}]}`)
		})

		t.Run("R5: an object whose only selections don't apply to its type", func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {"nodes": fn0(func() any {
					return list(obj{"__typename": "Article", "id": "a1", "title": "t"}, obj{"__typename": "Video", "id": "v1", "length": 1})
				})}},
				typeDefs: `
					type Query { nodes: [Node!]! }
					interface Node { id: ID! }
					type Article implements Node { id: ID! title: String! }
					type Video implements Node { id: ID! length: Int! }`,
			})
			query := `{ nodes { ... on Video { length } } }`
			expectData(t, h.run(query), `{"nodes":[{},{"length":1}]}`)
			expectData(t, h.run(query), `{"nodes":[{},{"length":1}]}`)
		})

		t.Run("R6: an entity TTL running out during a partial request isn't renewed", func(t *testing.T) {
			clk := newClock()
			state := map[string]int{"price": 1, "title": 1}
			g := newGate()
			var gated atomic.Bool
			h := newHarness(t, harnessOptions{
				cache:      b.newCache(t, clk.Now),
				now:        clk.Now,
				entityTTLs: map[string]time.Duration{"Price": 100 * time.Millisecond},
				resolvers: testexec.Resolvers{
					"Product": {"title": fn(func(p obj, _ obj) any {
						if gated.Load() {
							g.wait()
						}
						return p["title"]
					})},
					"Query": {
						"price":   fn0(func() any { return obj{"id": "p", "v": state["price"]} }),
						"product": fn0(func() any { return obj{"id": "x", "title": state["title"]} }),
					},
				},
				ttl: 10 * time.Second,
				typeDefs: `
					type Query { product: Product! price: Price! }
					type Product { id: ID! title: Int! }
					type Price { id: ID! v: Int! }`,
			})
			query := `{ product { id title } price { id v } }`
			h.run(query) // t=0, price v1 valid until t=100
			state["price"] = 2
			state["title"] = 2
			clk.Set(50 * time.Millisecond)
			h.invalidate(mangrove.Entity("Product", "x"))
			gated.Store(true)
			inflight := h.runAsync(query) // price still valid at read time
			<-g.started
			clk.Set(150 * time.Millisecond) // price's TTL runs out while the request is running
			gated.Store(false)
			g.open()
			inflight.wait(t)
			clk.Set(200 * time.Millisecond) // price data is 200ms old, its TTL is 100ms
			expectEqual(t, h.run(query).path(t, "price", "v"), 2.0)
		})

		t.Run("R7: resolutions drop fragments on non-entity union members", func(t *testing.T) {
			name := "a"
			ids := []string{"1"}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {
					"a": fn(func(_ obj, args obj) any {
						return obj{"__typename": "A", "id": args["id"], "name": fmt.Sprint(name, args["id"])}
					}),
					"things": fn0(func() any {
						var out []any
						for _, id := range ids {
							out = append(out, obj{"__typename": "A", "id": id, "name": name + id})
						}
						return append(out, obj{"__typename": "B", "name": "b"})
					}),
				}},
				typeDefs: `
					type Query { things: [Thing!]! a(id: ID!): A @cacheResolver }
					union Thing = A | B
					type A { id: ID! name: String! }
					type B { name: String! }`,
			})
			query := `{ things { ... on A { id name } ... on B { name } } }`
			h.run(query)
			name = "z"
			h.invalidate(mangrove.Entity("A", "1"))
			expectEqual(t, h.run(query).path(t, "things", 0, "name"), "z1")
			expectLen(t, h.logs.Warnings(), 0)
		})

		t.Run("R7b: resolutions drop fragments on types the resolver's type can't be", func(t *testing.T) {
			name := "a"
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {
					"a": fn(func(_ obj, args obj) any {
						return obj{"__typename": "A", "id": args["id"], "name": fmt.Sprint(name, args["id"])}
					}),
					"things": fn0(func() any { return list(obj{"__typename": "A", "id": "1", "name": name + "1"}) }),
				}},
				typeDefs: `
					type Query { things: [Thing!]! a(id: ID!): A @cacheResolver }
					union Thing = A | B
					interface Named { name: String! }
					type A implements Named { id: ID! name: String! }
					type B implements Named { id: ID! name: String! }`,
			})
			query := `{ things { ... on A { id } ... on B { id name } ... on Named { name } } }`
			h.run(query)
			name = "z"
			h.invalidate(mangrove.Entity("A", "1"))
			r := h.run(query)
			expectData(t, r, `{"things":[{"id":"1","name":"z1"}]}`)
			expectContains(t, r.executed[0], "... on Named")
			// The client's fragment on B can't apply to A and must be dropped.
			if regexp.MustCompile(`\.\.\. on B \{\s+id\s+name`).MatchString(r.executed[0]) {
				t.Fatalf("fragment on B kept:\n%s", r.executed[0])
			}
			expectLen(t, h.logs.Warnings(), 0)
		})

		t.Run("R8: results with errors are not cached", func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Query": {"item": fn0(func() any { return obj{"id": "1"} })},
					"Item": {"bad": fnCtx(func(context.Context, obj, obj) (any, error) {
						return nil, fmt.Errorf("boom")
					})},
				},
				typeDefs: `type Query { item: Item! } type Item { id: ID! bad: String }`,
			})
			q := `{ item { id bad } }`
			expectLen(t, h.run(q).response.Errors, 1)
			expectLen(t, h.run(q).response.Errors, 1)
		})

		t.Run("R9: a batch cache resolver omitting an entity doesn't leave stale data", func(t *testing.T) {
			items := map[string]obj{"1": {"id": "1", "name": "one"}, "2": {"id": "2", "name": "two"}}
			hidden := map[string]bool{}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {
					"items": fn0(func() any { return list(items["1"], items["2"]) }),
					"itemsByIds": fn(func(_ obj, args obj) any {
						var out []any
						for _, id := range args["ids"].([]any) {
							if !hidden[id.(string)] {
								out = append(out, items[id.(string)])
							}
						}
						return out
					}),
				}},
				typeDefs: `type Query { items: [Item!]! itemsByIds(ids: [ID!]!): [Item!]! @cacheResolver } type Item { id: ID! name: String! }`,
			})
			expectEqual(t, h.m.CacheResolvers()["Item"].Batch, true)
			q := `{ items { id name } }`
			h.run(q)
			items["1"]["name"] = "ONE"
			hidden["1"] = true // e.g. the batch resolver doesn't return it
			h.invalidate(mangrove.Entity("Item", "1"))
			expectEqual(t, h.run(q).path(t, "items", 0, "name"), "ONE")
			hidden = map[string]bool{}
			expectEqual(t, h.run(q).path(t, "items", 0, "name"), "ONE")
		})
	})
}
