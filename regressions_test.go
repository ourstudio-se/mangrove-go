package mangrove_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"sync/atomic"
	"time"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
)

const itemOwnerSchema = `
type Query {
  items: [Item!]!
  item(id: ID!): Item @cacheResolver
  featured: Item
}
type Item {
  id: ID!
  name: String!
  owner: Owner!
}
type Owner {
  id: ID!
  name: String!
}
`

type itemsWithOwners struct {
	owners    map[string]obj
	items     []obj
	calls     map[string]int
	featured  func() any
	resolvers testexec.Resolvers
}

func newItemsWithOwners() *itemsWithOwners {
	d := &itemsWithOwners{
		owners: map[string]obj{
			"5": {"id": "5", "name": "Alice"},
			"6": {"id": "6", "name": "Bob"},
		},
		items: []obj{
			{"id": "1", "name": "one", "ownerId": "5"},
			{"id": "2", "name": "two", "ownerId": "6"},
		},
		calls: map[string]int{},
	}
	d.featured = func() any { return d.items[0] }
	d.resolvers = testexec.Resolvers{
		"Item": {"owner": fn(func(item obj, _ obj) any { return d.owners[item["ownerId"].(string)] })},
		"Query": {
			"featured": fn0(func() any { return d.featured() }),
			"item": fnCtx(func(ctx context.Context, _ obj, args obj) (any, error) {
				if !isDirect(ctx) {
					d.calls["item"]++
				}
				for _, item := range d.items {
					if item["id"] == args["id"] {
						return item, nil
					}
				}
				return nil, nil
			}),
			"items": fnCtx(func(ctx context.Context, _ obj, _ obj) (any, error) {
				if !isDirect(ctx) {
					d.calls["items"]++
				}
				return list(d.items...), nil
			}),
		},
	}
	return d
}

func TestRegressions(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		t.Run("C2: invalidated entity below an entity with a cache resolver, in a list", func(t *testing.T) {
			d := newItemsWithOwners()
			h := newHarness(t, harnessOptions{cache: b.newCache(t, nil), resolvers: d.resolvers, typeDefs: itemOwnerSchema})
			query := `{ items { id name owner { id name } } }`

			h.run(query)
			d.owners["5"]["name"] = "Alice (renamed)"
			h.invalidate(mangrove.Entity("Owner", "5"))

			r := h.run(query)
			expectEqual(t, r.path(t, "items", 0, "owner", "name"), "Alice (renamed)")
			expectEqual(t, r.path(t, "items", 1, "owner", "name"), "Bob")
			expectLen(t, r.executed, 1)
			expectContains(t, r.executed[0], `item(id: "1")`)
			expectNotContains(t, r.executed[0], `item(id: "2")`)
			expectEqual(t, d.calls["items"], 1)
		})

		t.Run("C2: on a single object", func(t *testing.T) {
			d := newItemsWithOwners()
			h := newHarness(t, harnessOptions{cache: b.newCache(t, nil), resolvers: d.resolvers, typeDefs: itemOwnerSchema})
			query := `{ featured { id name owner { id name } } }`

			h.run(query)
			d.owners["5"]["name"] = "Alice (renamed)"
			h.invalidate(mangrove.Entity("Owner", "5"))

			r := h.run(query)
			expectEqual(t, r.path(t, "featured", "owner", "name"), "Alice (renamed)")
			expectContains(t, r.executed[0], `item(id: "1")`)
			h.expectNoErrors()
			expectLen(t, h.logs.Warnings(), 0)
		})

		for _, typename := range []string{"Apple", "Zebra"} {
			t.Run("C3: root invalidation with a list of "+typename, func(t *testing.T) {
				people := []obj{{"id": "1", "name": "Ann"}}
				h := newHarness(t, harnessOptions{
					cache:     b.newCache(t, nil),
					resolvers: testexec.Resolvers{"Query": {"list": fn0(func() any { return list(people...) })}},
					typeDefs:  fmt.Sprintf(`type Query { list: [%[1]s!]! } type %[1]s { id: ID! name: String! }`, typename),
				})
				query := `{ list { id name } }`

				h.run(query)
				people = append(people, obj{"id": "2", "name": "Bob"})
				h.invalidate(mangrove.EntityType("Query"))

				expectData(t, h.run(query), `{"list":[{"id":"1","name":"Ann"},{"id":"2","name":"Bob"}]}`)
			})
		}

		t.Run("C4: invalidating an entity in a nested list", func(t *testing.T) {
			items := []obj{{"data": "v1", "id": "1"}}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Dashboard": {"items": fn0(func() any { return list(items...) })},
					"Query":     {"dashboard": fn0(func() any { return obj{"id": "d"} })},
				},
				typeDefs: `
					type Query { dashboard: Dashboard! }
					type Dashboard { id: ID! items: [Item!]! }
					type Item { id: ID! data: String! }`,
			})
			query := `{ dashboard { id items { id data } } }`

			h.run(query)
			items[0]["data"] = "v2"
			h.invalidate(mangrove.Entity("Item", "1"))

			expectEqual(t, h.run(query).path(t, "dashboard", "items", 0, "data"), "v2")
		})

		t.Run("C6: an invalidation during a request is not undone by its store", func(t *testing.T) {
			item := obj{"data": "v1", "id": "1"}
			g := newGate()
			var gated atomic.Bool
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Item": {"data": fn(func(parent obj, _ obj) any {
						value := parent["data"]
						if gated.Load() {
							g.wait()
						}
						return value
					})},
					"Query": {"item": fn0(func() any { return copyObj(item) })},
				},
				typeDefs: `type Query { item: Item! } type Item { id: ID! data: String! }`,
			})
			query := `{ item { id data } }`

			gated.Store(true)
			inFlight := h.runAsync(query)
			<-g.started

			// The source changes while the request has already read v1.
			item["data"] = "v2"
			h.invalidate(mangrove.Entity("Item", "1"))

			gated.Store(false)
			g.open()
			expectEqual(t, inFlight.wait(t).path(t, "item", "data"), "v1")

			expectEqual(t, h.run(query).path(t, "item", "data"), "v2")
		})

		t.Run("C6: a slow request can't overwrite a newer result with stale data", func(t *testing.T) {
			item := obj{"data": "v1", "id": "1"}
			g := newGate()
			var gateNext atomic.Bool
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Item": {"data": fn(func(parent obj, _ obj) any {
						value := parent["data"]
						if parent["gated"] == true {
							g.wait()
						}
						return value
					})},
					"Query": {"item": fn0(func() any {
						copied := copyObj(item)
						copied["gated"] = gateNext.Swap(false)
						return copied
					})},
				},
				typeDefs: `type Query { item: Item! } type Item { id: ID! data: String! }`,
			})
			query := `{ item { id data } }`

			// A: starts before the change and is slow.
			gateNext.Store(true)
			slow := h.runAsync(query)
			<-g.started

			item["data"] = "v2"
			h.invalidate(mangrove.Entity("Item", "1"))

			// B: starts after the change and stores first.
			expectEqual(t, h.run(query).path(t, "item", "data"), "v2")

			// A stores last.
			g.open()
			expectEqual(t, slow.wait(t).path(t, "item", "data"), "v1")

			expectEqual(t, h.run(query).path(t, "item", "data"), "v2")
		})

		t.Run("C6: cache hits don't write back, and don't extend the expiry", func(t *testing.T) {
			clk := newClock()
			cache := &countingCache{Cache: b.newCache(t, clk.Now)}
			version := 1
			h := newHarness(t, harnessOptions{
				cache:     cache,
				now:       clk.Now,
				resolvers: testexec.Resolvers{"Query": {"item": fn0(func() any { return obj{"id": "1", "v": version} })}},
				ttl:       time.Second,
				typeDefs:  `type Query { item: Item! } type Item { id: ID! v: Int! }`,
			})
			query := `{ item { id v } }`

			h.run(query)
			expectEqual(t, cache.sets.Load(), int64(1))

			version = 2
			clk.Set(500 * time.Millisecond)
			hit := h.run(query)
			expectLen(t, hit.executed, 0)
			expectEqual(t, hit.path(t, "item", "v"), 1.0)
			expectEqual(t, cache.sets.Load(), int64(1))

			clk.Set(1100 * time.Millisecond)
			expired := h.run(query)
			expectEqual(t, expired.hit(), false)
			expectEqual(t, expired.path(t, "item", "v"), 2.0)
		})

		t.Run("C6: expiry counts from when the data was read, not when it was stored", func(t *testing.T) {
			clk := newClock()
			version := 1
			g := newGate()
			var gated atomic.Bool
			gated.Store(true)
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, clk.Now),
				now:   clk.Now,
				resolvers: testexec.Resolvers{
					"Item": {"v": fn(func(item obj, _ obj) any {
						if gated.Load() {
							g.wait()
						}
						return item["v"]
					})},
					"Query": {"item": fn0(func() any { return obj{"id": "1", "v": version} })},
				},
				ttl:      time.Second,
				typeDefs: `type Query { item: Item! } type Item { id: ID! v: Int! }`,
			})
			query := `{ item { id v } }`

			// Reads v1 at t=0, stores at t=300.
			slow := h.runAsync(query)
			<-g.started
			clk.Set(300 * time.Millisecond)
			gated.Store(false)
			g.open()
			slow.wait(t)

			version = 2
			clk.Set(1050 * time.Millisecond)
			r := h.run(query)
			expectEqual(t, r.hit(), false)
			expectEqual(t, r.path(t, "item", "v"), 2.0)
		})

		t.Run("C6: a partially refreshed result keeps its original expiry", func(t *testing.T) {
			clk := newClock()
			state := map[string]int{"a": 1, "b": 1}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, clk.Now),
				now:   clk.Now,
				resolvers: testexec.Resolvers{"Query": {
					"a": fn0(func() any { return obj{"id": "a", "v": state["a"]} }),
					"b": fn0(func() any { return obj{"id": "b", "v": state["b"]} }),
				}},
				ttl:      time.Second,
				typeDefs: `type Query { a: T! b: T! } type T { id: ID! v: Int! }`,
			})
			query := `{ a { id v } b { id v } }`

			h.run(query)
			state["a"] = 2
			state["b"] = 2
			h.invalidate(mangrove.Entity("T", "a"))

			clk.Set(300 * time.Millisecond)
			expectData(t, h.run(query), `{"a":{"id":"a","v":2},"b":{"id":"b","v":1}}`)

			// b is as old as the original result: it must not outlive it.
			clk.Set(1100 * time.Millisecond)
			expectData(t, h.run(query), `{"a":{"id":"a","v":2},"b":{"id":"b","v":2}}`)
		})

		t.Run("C6: per-entity TTLs are honoured", func(t *testing.T) {
			clk := newClock()
			state := map[string]int{"price": 1, "title": 1}
			h := newHarness(t, harnessOptions{
				cache:      b.newCache(t, clk.Now),
				now:        clk.Now,
				entityTTLs: map[string]time.Duration{"Price": 100 * time.Millisecond},
				resolvers: testexec.Resolvers{"Query": {
					"price":   fn0(func() any { return obj{"id": "p", "v": state["price"]} }),
					"product": fn0(func() any { return obj{"id": "x", "title": state["title"]} }),
				}},
				ttl: 10 * time.Second,
				typeDefs: `
					type Query { product: Product! price: Price! }
					type Product { id: ID! title: Int! }
					type Price { id: ID! v: Int! }`,
			})
			query := `{ product { id title } price { id v } }`

			h.run(query)
			state["price"] = 2
			state["title"] = 2

			clk.Set(50 * time.Millisecond)
			expectData(t, h.run(query), `{"price":{"id":"p","v":1},"product":{"id":"x","title":1}}`)

			// A partial refresh of the product stores the result again, but
			// must not extend how long the (unchanged) price is considered
			// valid.
			h.invalidate(mangrove.Entity("Product", "x"))
			clk.Set(60 * time.Millisecond)
			expectData(t, h.run(query), `{"price":{"id":"p","v":1},"product":{"id":"x","title":2}}`)

			clk.Set(120 * time.Millisecond)
			expectData(t, h.run(query), `{"price":{"id":"p","v":2},"product":{"id":"x","title":2}}`)
		})

		t.Run("C7: a link to an entity known elsewhere in the result", func(t *testing.T) {
			d := newItemsWithOwners()
			featured := d.items[0]
			d.featured = func() any { return featured }
			h := newHarness(t, harnessOptions{cache: b.newCache(t, nil), resolvers: d.resolvers, typeDefs: itemOwnerSchema})
			query := `{ featured { id name } items { id name } }`

			h.run(query)
			featured = d.items[1]
			h.invalidate(mangrove.EntityType("Query"))

			expected := `{"featured":{"id":"2","name":"two"},"items":[{"id":"1","name":"one"},{"id":"2","name":"two"}]}`
			expectData(t, h.run(query), expected)
			// And it stays correct when served from the cache.
			expectData(t, h.run(query), expected)
		})

		t.Run("C7: links swapped between positions at the same coordinates", func(t *testing.T) {
			items := map[string]obj{"1": {"id": "1", "name": "one"}, "2": {"id": "2", "name": "two"}}
			users := []obj{{"fav": "1", "id": "u1"}, {"fav": "2", "id": "u2"}}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Query": {
						"item":  fn(func(_ obj, args obj) any { return items[args["id"].(string)] }),
						"users": fn0(func() any { return list(users...) }),
					},
					"User": {"fav": fn(func(user obj, _ obj) any { return items[user["fav"].(string)] })},
				},
				typeDefs: `
					type Query { users: [User!]! item(id: ID!): Item @cacheResolver }
					type User { id: ID! fav: Item! }
					type Item { id: ID! name: String! }`,
			})
			query := `{ users { id fav { id name } } }`

			h.run(query)
			users[0]["fav"] = "2"
			users[1]["fav"] = "1"
			h.invalidate(mangrove.EntityType("User"))

			r := h.run(query)
			expectData(t, r, `{"users":[{"fav":{"id":"2","name":"two"},"id":"u1"},{"fav":{"id":"1","name":"one"},"id":"u2"}]}`)
			// Both items are known at users.fav: no link queries needed.
			expectLen(t, r.executed, 1)
		})

		t.Run("C7: entities of different types with the same id", func(t *testing.T) {
			results := []obj{{"__typename": "A", "a": "alpha", "id": "1"}, {"__typename": "B", "b": "beta", "id": "1"}}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {
					"a":       fn0(func() any { return results[0] }),
					"b":       fn0(func() any { return results[1] }),
					"results": fn0(func() any { return list(results...) }),
				}},
				typeDefs: `
					type Query { results: [R!]! a(id: ID!): A @cacheResolver b(id: ID!): B @cacheResolver }
					union R = A | B
					type A { id: ID! a: String! }
					type B { id: ID! b: String! }`,
			})
			query := `{ results { ... on A { id a } ... on B { id b } } }`

			h.run(query)
			results = []obj{results[1], results[0]}
			h.invalidate(mangrove.EntityType("Query"))

			expectData(t, h.run(query), `{"results":[{"b":"beta","id":"1"},{"a":"alpha","id":"1"}]}`)
		})

		t.Run("H1: link queries carry the variables they use", func(t *testing.T) {
			items := []obj{{"id": "1"}, {"id": "2"}}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Item": {"data": fn(func(item obj, args obj) any { return fmt.Sprintf("%v:%v", item["id"], args["n"]) })},
					"Query": {
						"item":  fn(func(_ obj, args obj) any { return findByID(items, args["id"]) }),
						"items": fn0(func() any { return list(items...) }),
					},
				},
				typeDefs: `
					type Query { items: [Item!]! item(id: ID!): Item @cacheResolver }
					type Item { id: ID! data(n: Int): String! }`,
			})
			query := `query Q($n: Int) { items { id data(n: $n) } }`
			variables := runOptions{variables: map[string]any{"n": 7}}

			h.run(query, variables)
			items = append(items, obj{"id": "3"})
			h.invalidate(mangrove.EntityType("Query"))

			r := h.run(query, variables)
			expectData(t, r, `{"items":[{"id":"1","data":"1:7"},{"id":"2","data":"2:7"},{"id":"3","data":"3:7"}]}`)
			expectContains(t, r.executed[1], "query Q__linkQuery($n: Int)")
		})

		t.Run("H1: unused variable definitions are removed from partial queries", func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Item": {"data": fn(func(_ obj, args obj) any { return fmt.Sprintf("d%v", args["n"]) })},
					"Query": {
						"item":  fn0(func() any { return obj{"id": "1"} }),
						"items": fn0(func() any { return list(obj{"id": "2"}) }),
					},
				},
				typeDefs: `
					type Query { items: [Item!]! item(id: ID!): Item @cacheResolver }
					type Item { id: ID! data(n: Int): String! }`,
			})
			query := `query Q($n: Int) { a: item(id: "1") { id data(n: $n) } items { id } }`
			variables := runOptions{variables: map[string]any{"n": 1}}

			h.run(query, variables)
			h.invalidate(mangrove.EntityType("Query"))

			// The executor rejects invalid documents (e.g. unused variables).
			r := h.run(query, variables)
			expectLen(t, r.executed, 1)
			expectNotContains(t, r.executed[0], "$n")
			expectData(t, r, `{"a":{"data":"d1","id":"1"},"items":[{"id":"2"}]}`)
		})

		t.Run("H2: documents with several operations", func(t *testing.T) {
			item := obj{"id": "1", "name": "v1"}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {
					"item":  fn0(func() any { return copyObj(item) }),
					"other": fn0(func() any { return "x" }),
				}},
				typeDefs: `type Query { item: Item! other: String! } type Item { id: ID! name: String! }`,
			})
			query := `
				query GetItem { item { id name } }
				query GetOther { other }`

			h.run(query, runOptions{operationName: "GetItem"})
			item["name"] = "v2"
			h.invalidate(mangrove.Entity("Item", "1"))

			expectData(t, h.run(query, runOptions{operationName: "GetItem"}), `{"item":{"id":"1","name":"v2"}}`)
			expectData(t, h.run(query, runOptions{operationName: "GetOther"}), `{"other":"x"}`)
		})

		nodeSchema := `
			type Query {
				node(id: ID!): Node @cacheResolver
				search: [SearchResult!]!
				nodes: [Node!]!
			}
			interface Node { id: ID! }
			union SearchResult = Article | Video
			type Article implements Node { id: ID! title: String! }
			type Video implements Node { id: ID! length: Int! }`

		nodeData := func() (map[string]obj, testexec.Resolvers) {
			nodes := map[string]obj{
				"a1": {"__typename": "Article", "id": "a1", "title": "First"},
				"v1": {"__typename": "Video", "id": "v1", "length": 10},
			}
			all := func() any { return list(nodes["a1"], nodes["v1"]) }
			return nodes, testexec.Resolvers{"Query": {
				"node": fn(func(_ obj, args obj) any {
					if node, ok := nodes[args["id"].(string)]; ok {
						return node
					}
					return nil
				}),
				"nodes":  fn0(all),
				"search": fn0(all),
			}}
		}

		t.Run("H3: interface-typed fields identify their entities", func(t *testing.T) {
			nodes, resolvers := nodeData()
			h := newHarness(t, harnessOptions{cache: b.newCache(t, nil), resolvers: resolvers, typeDefs: nodeSchema})
			query := `{ nodes { id ... on Article { title } } }`

			h.run(query)
			nodes["a1"]["title"] = "Updated"
			h.invalidate(mangrove.Entity("Article", "a1"))

			expectEqual(t, h.run(query).path(t, "nodes", 0, "title"), "Updated")
			h.expectNoErrors()
		})

		t.Run("H3: union lists are re-resolved per type through an interface resolver", func(t *testing.T) {
			nodes, resolvers := nodeData()
			h := newHarness(t, harnessOptions{cache: b.newCache(t, nil), resolvers: resolvers, typeDefs: nodeSchema})
			query := `{ search { ... on Article { id title } ... on Video { id length } } }`

			expectEqual(t, h.m.CacheResolvers()["Article"].RootField, "node")
			expectEqual(t, h.m.CacheResolvers()["Video"].RootField, "node")

			h.run(query)
			nodes["v1"]["length"] = 20
			h.invalidate(mangrove.Entity("Video", "v1"))

			r := h.run(query)
			expectData(t, r, `{"search":[{"id":"a1","title":"First"},{"id":"v1","length":20}]}`)
			expectContains(t, r.executed[0], `node(id: "v1")`)
			expectNotContains(t, r.executed[0], "... on Article")
			expectLen(t, h.logs.Warnings(), 0)
		})

		for _, id := range []string{"a.b", `x"y`, `C:\new`, "urn:x:1", "user@example.com", "a>b", "#1", "0", "tab\there", "\x01ctl", "💥", ""} {
			t.Run(fmt.Sprintf("H4: id %q", id), func(t *testing.T) {
				items := map[string]obj{id: {"id": id, "v": 1}}
				h := newHarness(t, harnessOptions{
					cache: b.newCache(t, nil),
					resolvers: testexec.Resolvers{"Query": {
						"item": fn(func(_ obj, args obj) any {
							if item, ok := items[args["id"].(string)]; ok {
								return item
							}
							return nil
						}),
						"items": fn0(func() any { return list(items[id]) }),
					}},
					typeDefs: `
						type Query { items: [Item!]! item(id: ID!): Item @cacheResolver }
						type Item { id: ID! v: Int! }`,
				})
				query := `{ items { id v } }`

				h.run(query)
				items[id]["v"] = 2
				h.invalidate(mangrove.Entity("Item", id))

				r := h.run(query)
				encodedID, _ := json.Marshal(id)
				expectData(t, r, fmt.Sprintf(`{"items":[{"id":%s,"v":2}]}`, encodedID))
				expectLen(t, r.executed, 1)
				expectEqual(t, r.partial(), true)
				h.expectNoErrors()
			})
		}

		t.Run("H4: field names containing underscores", func(t *testing.T) {
			item := obj{"id": "1", "v": 1}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {
					"item":     fn0(func() any { return copyObj(item) }),
					"top_item": fn0(func() any { return copyObj(item) }),
				}},
				typeDefs: `
					type Query { top_item: Item! item(id: ID!): Item @cacheResolver }
					type Item { id: ID! v: Int! }`,
			})
			query := `{ top_item { id v } }`

			h.run(query)
			item["v"] = 2
			h.invalidate(mangrove.Entity("Item", "1"))

			r := h.run(query)
			expectData(t, r, `{"top_item":{"id":"1","v":2}}`)
			expectContains(t, r.executed[0], "item(id:")
		})

		t.Run("H5: lists containing null", func(t *testing.T) {
			items := []any{obj{"id": "1", "v": "one"}, nil, obj{"id": "2", "v": "two"}}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {
					"item": fn(func(_ obj, args obj) any {
						for _, item := range items {
							if o, ok := item.(obj); ok && o["id"] == args["id"] {
								return o
							}
						}
						return nil
					}),
					"list": fn0(func() any { return items }),
				}},
				typeDefs: `
					type Query { list: [Item]! item(id: ID!): Item @cacheResolver }
					type Item { id: ID! v: String! }`,
			})
			query := `{ list { id v } }`

			h.run(query)
			items = []any{items[2], nil, items[0]}
			h.invalidate(mangrove.EntityType("Query"))

			expectData(t, h.run(query), `{"list":[{"id":"2","v":"two"},null,{"id":"1","v":"one"}]}`)
		})

		t.Run("H5: a cache resolver returning null falls back to full execution", func(t *testing.T) {
			d := newItemsWithOwners()
			h := newHarness(t, harnessOptions{cache: b.newCache(t, nil), resolvers: d.resolvers, typeDefs: itemOwnerSchema})
			query := `{ items { id name } }`

			h.run(query)
			// Item 1 is deleted, but only the item is invalidated.
			d.items = d.items[1:]
			h.invalidate(mangrove.Entity("Item", "1"))

			expectData(t, h.run(query), `{"items":[{"id":"2","name":"two"}]}`)
			if len(h.logs.Warnings()) == 0 {
				t.Fatal("expected a warning about the fallback")
			}
		})

		t.Run("H5: an object that changed type doesn't keep fields of the old type", func(t *testing.T) {
			thing := obj{"__typename": "A", "a": "aa"}
			h := newHarness(t, harnessOptions{
				cache:     b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {"thing": fn0(func() any { return thing })}},
				typeDefs: `
					type Query { thing: Thing! }
					union Thing = A | B
					type A { a: String! }
					type B { b: String! }`,
			})
			query := `{ thing { ... on A { a } ... on B { b } } }`

			h.run(query)
			thing = obj{"__typename": "B", "b": "bb"}
			h.invalidate(mangrove.EntityType("Query"))

			expectData(t, h.run(query), `{"thing":{"b":"bb"}}`)
		})

		t.Run("H6: Int ids of known entities are not refetched", func(t *testing.T) {
			items := []obj{{"id": 1, "v": "one"}, {"id": 2, "v": "two"}}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {
					"item": fn(func(_ obj, args obj) any {
						for _, item := range items {
							if int64(item["id"].(int)) == args["id"] {
								return item
							}
						}
						return nil
					}),
					"items": fn0(func() any { return list(items...) }),
				}},
				typeDefs: `
					type Query { items: [Item!]! item(id: Int!): Item @cacheResolver }
					type Item { id: Int! v: String! }`,
			})
			query := `{ items { id v } }`

			h.run(query)
			items = []obj{items[1], items[0]}
			h.invalidate(mangrove.EntityType("Query"))

			r := h.run(query)
			expectData(t, r, `{"items":[{"id":2,"v":"two"},{"id":1,"v":"one"}]}`)
			expectLen(t, r.executed, 1)
		})

		t.Run("errors in partial executions are reported against the original query", func(t *testing.T) {
			fail := false
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Item": {"v": fnCtx(func(context.Context, obj, obj) (any, error) {
						if fail {
							return nil, fmt.Errorf("boom")
						}
						return 1, nil
					})},
					"Query": {
						"item":  fn0(func() any { return obj{"id": "1"} }),
						"items": fn0(func() any { return list(obj{"id": "1"}) }),
					},
				},
				typeDefs: `
					type Query { items: [Item!]! item(id: ID!): Item @cacheResolver }
					type Item { id: ID! v: Int }`,
			})
			query := `{ items { id v } }`

			h.run(query)
			fail = true
			h.invalidate(mangrove.Entity("Item", "1"))

			r := h.run(query)
			expectEqual(t, r.response.Errors[0].Path.String(), "items[0].v")
			expectData(t, r, `{"items":[{"id":"1","v":null}]}`)
		})

		t.Run("mutations are executed every time without marker fields", func(t *testing.T) {
			counter := 0
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Mutation": {"bump": fn0(func() any { counter++; return obj{"id": "1", "n": counter} })},
					"Query":    {"x": fn0(func() any { return 1 })},
				},
				typeDefs: `
					type Query { x: Int }
					type Mutation { bump: Counter! }
					type Counter { id: ID! n: Int! }`,
			})
			mutation := `
				fragment C on Counter { id n }
				mutation M { bump { ...C } }`

			expectEqual(t, h.run(mutation).data(), `{"bump":{"id":"1","n":1}}`)
			expectEqual(t, h.run(mutation).data(), `{"bump":{"id":"1","n":2}}`)
		})

		t.Run("disabled requests keep their operationName and lose marker fields", func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				cache:   b.newCache(t, nil),
				enabled: func(context.Context) bool { return false },
				resolvers: testexec.Resolvers{"Query": {
					"a": fn0(func() any { return obj{"id": "1"} }),
					"b": fn0(func() any { return 2 }),
				}},
				typeDefs: `type Query { a: A! b: Int! } type A { id: ID! }`,
			})

			r := h.run(`query A { a { id } } query B { b }`, runOptions{operationName: "A"})
			expectEqual(t, r.json(), `{"data":{"a":{"id":"1"}}}`)
		})

		t.Run("invalidating a whole type only affects that type", func(t *testing.T) {
			state := map[string]int{"group": 1, "item": 1}
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{"Query": {
					"group": fn0(func() any { return obj{"id": "1", "v": state["group"]} }),
					"item":  fn0(func() any { return obj{"id": "1", "v": state["item"]} }),
				}},
				typeDefs: `
					type Query { item: Item! group: ItemGroup! }
					type Item { id: ID! v: Int! }
					type ItemGroup { id: ID! v: Int! }`,
			})
			query := `{ item { id v } group { id v } }`

			h.run(query)
			state["item"] = 2
			state["group"] = 2
			h.invalidate(mangrove.EntityType("Item"))

			expectData(t, h.run(query), `{"group":{"id":"1","v":1},"item":{"id":"1","v":2}}`)
		})

		t.Run("invalidating rejects an empty typename", func(t *testing.T) {
			h := newHarness(t, harnessOptions{cache: b.newCache(t, nil), typeDefs: `type Query { x: Int }`})
			if err := h.strategy.InvalidateEntities(context.Background(), []mangrove.EntityRecord{{Typename: ""}}); err == nil {
				t.Fatal("expected an error")
			}
		})

		t.Run("the README walkthrough", func(t *testing.T) {
			items := []obj{{"id": "1"}, {"id": "2"}}
			generation := 0
			var dataCalls []string
			h := newHarness(t, harnessOptions{
				cache: b.newCache(t, nil),
				resolvers: testexec.Resolvers{
					"Item": {"data": fn(func(item obj, _ obj) any {
						dataCalls = append(dataCalls, item["id"].(string))
						return fmt.Sprintf("%v-%d", item["id"], generation)
					})},
					"Query": {
						"item":  fn(func(_ obj, args obj) any { return findByID(items, args["id"]) }),
						"items": fn0(func() any { return list(items...) }),
					},
				},
				typeDefs: `
					type Query { items: [Item!]! item(id: ID!): Item @cacheResolver }
					type Item { id: ID! data: String! }`,
			})
			query := `query BigItemQuery { items { id data } }`
			data := func() string {
				var out []string
				for _, item := range h.run(query).path(t, "items").([]any) {
					out = append(out, item.(map[string]any)["data"].(string))
				}
				return strings.Join(out, ",")
			}

			expectEqual(t, data(), "1-0,2-0")
			generation = 1
			expectEqual(t, data(), "1-0,2-0")

			h.invalidate(mangrove.Entity("Item", "1"))
			dataCalls = nil
			expectEqual(t, data(), "1-1,2-0")
			expectEqual(t, dataCalls, []string{"1"})

			items = append(items, obj{"id": "3"})
			h.invalidate(mangrove.EntityType("Query"))
			dataCalls = nil
			expectEqual(t, data(), "1-1,2-0,3-1")
			expectEqual(t, dataCalls, []string{"3"})

			items = append(items[1:], items[0])
			h.invalidate(mangrove.EntityType("Query"))
			dataCalls = nil
			expectEqual(t, data(), "2-0,3-1,1-1")
			expectEqual(t, len(dataCalls), 0)
		})
	})
}

func findByID(items []obj, id any) any {
	for _, item := range items {
		if item["id"] == id {
			return item
		}
	}
	return nil
}
