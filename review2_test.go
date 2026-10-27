package mangrove_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
)

// Findings of a second independent review of the TS rework.
func TestReview2(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		priceSchema := `
			type Query { product: Product! price: Price! }
			type Product { id: ID! title: Int! }
			type Price { id: ID! v: Int! }`

		t.Run("F1: an older request at the same epoch doesn't overwrite a newer result after an entity TTL ran out", func(t *testing.T) {
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
						if gated.CompareAndSwap(true, false) {
							g.wait()
						}
						return p["title"]
					})},
					"Query": {
						"price":   fn0(func() any { return obj{"id": "p", "v": state["price"]} }),
						"product": fn0(func() any { return obj{"id": "x", "title": state["title"]} }),
					},
				},
				ttl:      10 * time.Second,
				typeDefs: priceSchema,
			})
			query := `{ product { id title } price { id v } }`
			h.run(query) // t=0: price v1, valid until t=100
			clk.Set(50 * time.Millisecond)
			state["title"] = 2
			h.invalidate(mangrove.Entity("Product", "x"))
			gated.Store(true)
			// A: price still valid at read time, refetches only the product.
			a := h.runAsync(query)
			<-g.started
			clk.Set(150 * time.Millisecond) // price's TTL runs out; price changes (TTL-driven data)
			state["price"] = 2
			// B: same epoch as A, started later; refetches price (v2) and product.
			expectEqual(t, h.run(query).path(t, "price", "v"), 2.0)
			g.open()
			a.wait(t) // A must not store price v1 from the cache over B's result
			clk.Set(160 * time.Millisecond)
			expectEqual(t, h.run(query).path(t, "price", "v"), 2.0)
		})

		t.Run("F2: a nested inline fragment without type condition drops fragments on other union members", func(t *testing.T) {
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
					type A { id: ID! name: String! }
					type B { id: ID! other: String! }`,
			})
			query := `query Q($yes: Boolean = true) { things { ... @include(if: $yes) { ... on A { id name } ... on B { id other } } } }`
			h.run(query)
			name = "z"
			h.invalidate(mangrove.Entity("A", "1"))
			expectData(t, h.run(query), `{"things":[{"id":"1","name":"z1"}]}`)
			h.expectNoErrors()
			expectLen(t, h.logs.Warnings(), 0)
		})

		t.Run("F3: an interface cache resolver for an entity at a concrete-typed position", func(t *testing.T) {
			name := "a"
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {
					"item": fn0(func() any { return obj{"__typename": "A", "id": "1", "name": name} }),
					"node": fn(func(_ obj, args obj) any { return obj{"__typename": "A", "id": args["id"], "name": name} }),
				}},
				typeDefs: `
					type Query { item: A! node(id: ID!): Node @cacheResolver }
					interface Node { id: ID! }
					type A implements Node { id: ID! name: String! }`,
			})
			query := `{ item { id name } }`
			h.run(query)
			name = "b"
			h.invalidate(mangrove.Entity("A", "1"))
			expectData(t, h.run(query), `{"item":{"id":"1","name":"b"}}`)
			expectLen(t, h.logs.Warnings(), 0)
		})

		t.Run("F4: errors cached with a result don't survive the erroring field being refetched", func(t *testing.T) {
			fail := true
			h := newHarness(t, harnessOptions{
				cache:             b.newCache(t, nil),
				noMetadata:        true,
				shouldCacheResult: func(string, *mangrove.CachedResult) bool { return true },
				resolvers: testexec.Resolvers{
					"Item": {"bad": fnCtx(func(context.Context, obj, obj) (any, error) {
						if fail {
							return nil, fmt.Errorf("boom")
						}
						return "ok", nil
					})},
					"Query": {
						"item":  fn(func(_ obj, args obj) any { return obj{"id": args["id"]} }),
						"items": fn0(func() any { return list(obj{"id": "1"}) }),
					},
				},
				typeDefs: `type Query { items: [Item!]! item(id: ID!): Item @cacheResolver } type Item { id: ID! bad: String }`,
			})
			query := `{ items { id bad } }`
			expectLen(t, h.run(query).response.Errors, 1)
			fail = false
			h.invalidate(mangrove.Entity("Item", "1"))
			second := h.run(query)
			expectData(t, second, `{"items":[{"bad":"ok","id":"1"}]}`)
			expectLen(t, second.response.Errors, 0)
		})

		t.Run("F5: links with typed id markers (nullable ID, Int!) in a union list", func(t *testing.T) {
			type entry struct {
				typename string
				id       any
			}
			entries := []entry{{"A", "1"}, {"B", 2}}
			rev := "r1"
			name := func(o obj) any { return fmt.Sprintf("%s%v%s", o["__typename"], o["id"], rev) }
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Query": {
						"a": fn(func(_ obj, args obj) any { return obj{"__typename": "A", "id": args["id"]} }),
						"b": fn(func(_ obj, args obj) any { return obj{"__typename": "B", "id": args["id"]} }),
						"things": fn0(func() any {
							var out []any
							for _, e := range entries {
								out = append(out, obj{"__typename": e.typename, "id": e.id})
							}
							return out
						}),
					},
					"A": {"name": fn(func(o obj, _ obj) any { return name(o) })},
					"B": {"name": fn(func(o obj, _ obj) any { return name(o) })},
				},
				typeDefs: `
					type Query { things: [Thing!]! a(id: ID!): A @cacheResolver b(id: Int!): B @cacheResolver }
					union Thing = A | B
					type A { id: ID name: String! }
					type B { id: Int! name: String! }`,
			})
			query := `{ things { ... on A { aid: id name } ... on B { bid: id name } } }`
			h.run(query)
			rev = "r2" // not invalidated: cached names must stay r1
			entries = []entry{{"B", 2}, {"A", "3"}, {"A", "1"}, {"B", 4}}
			h.invalidate(mangrove.EntityType("Query"))
			r := h.run(query)
			expectData(t, r, `{"things":[{"bid":2,"name":"B2r1"},{"aid":"3","name":"A3r2"},{"aid":"1","name":"A1r1"},{"bid":4,"name":"B4r2"}]}`)
			expectLen(t, h.logs.Warnings(), 0)
			expectLen(t, r.executed, 2)
		})

		t.Run("F1b: the same race with an entity key deleted directly", func(t *testing.T) {
			state := map[string]int{"price": 1, "title": 1}
			g := newGate()
			var gated atomic.Bool
			cache := b.newCache(t, nil)
			h := newHarness(t, harnessOptions{
				cache: cache,
				resolvers: testexec.Resolvers{
					"Product": {"title": fn(func(p obj, _ obj) any {
						if gated.CompareAndSwap(true, false) {
							g.wait()
						}
						return p["title"]
					})},
					"Query": {
						"price":   fn0(func() any { return obj{"id": "p", "v": state["price"]} }),
						"product": fn0(func() any { return obj{"id": "x", "title": state["title"]} }),
					},
				},
				typeDefs: priceSchema,
			})
			query := `{ product { id title } price { id v } }`
			h.run(query)
			state["title"] = 2
			h.invalidate(mangrove.Entity("Product", "x"))
			gated.Store(true)
			a := h.runAsync(query) // price valid at read
			<-g.started
			state["price"] = 2
			// "In a pinch, deleting the entity's key directly"
			if err := cache.Clear(context.Background(), []string{"Price:p"}); err != nil {
				t.Fatal(err)
			}
			expectEqual(t, h.run(query).path(t, "price", "v"), 2.0)
			g.open()
			a.wait(t)
			expectEqual(t, h.run(query).path(t, "price", "v"), 2.0)
		})
	})
}
