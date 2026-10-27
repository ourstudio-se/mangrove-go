package mangrove_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
)

// Findings of the differential fuzzers, as regression tests.

func TestSelfReferencingEntities(t *testing.T) {
	variants := [][2]string{
		{"A>A", "D>D"},
		{"A>A", "D>E"},
		{"A>B,B>B", "D>D"},
		{"A>B,B>A", "D>D"},
		{"A>A", "D>A"},
		{"A>B,B>C", "D>B"},
	}

	for _, resolver := range []string{"book", "node"} {
		for _, variant := range variants {
			initial, after := variant[0], variant[1]
			t.Run(fmt.Sprintf("%s: initially %s, then A>D with %s", resolver, initial, after), func(t *testing.T) {
				sequels := map[string]string{}
				for _, pair := range strings.Split(initial, ",") {
					sequels[pair[:1]] = pair[2:]
				}
				var book func(id string) any
				book = func(id string) any {
					if id == "" {
						return nil
					}
					return obj{"__typename": "Book", "id": id, "name": "n" + id, "sequel": sequels[id]}
				}
				directive := func(name string) string {
					if name == resolver {
						return "@cacheResolver"
					}
					return ""
				}
				h := newHarness(t, harnessOptions{
					cache: memoryBackend().newCache(t, nil),
					resolvers: testexec.Resolvers{
						"Book": {"sequel": fn(func(b obj, _ obj) any { return book(b["sequel"].(string)) })},
						"Query": {
							"best": fn0(func() any { return book("A") }),
							"book": fn(func(_ obj, args obj) any { return book(args["id"].(string)) }),
							"node": fn(func(_ obj, args obj) any { return book(args["id"].(string)) }),
						},
					},
					typeDefs: fmt.Sprintf(`
						interface Node { id: ID! }
						type Query {
							best: Book
							book(id: ID!): Book %s
							node(id: ID!): Node %s
						}
						type Book implements Node { id: ID! name: String! sequel: Book }`, directive("book"), directive("node")),
				})
				query := `{ best { id sequel { id sequel { id name } } } }`

				h.expectFresh(query)
				sequels["A"] = "D"
				sequels["D"] = after[2:]
				h.invalidate(mangrove.Entity("Book", "A"), mangrove.Entity("Book", "D"))
				h.expectFresh(query)
				h.expectFresh(query)
			})
		}
	}
}

// gatedCache pauses the first read of entity references after pause().
type gatedCache struct {
	mangrove.Cache
	mu      sync.Mutex
	gate    chan struct{}
	arrived chan struct{}
}

func (c *gatedCache) GetSetMembers(ctx context.Context, key string) ([]string, error) {
	c.mu.Lock()
	gate, arrived := c.gate, c.arrived
	c.gate = nil
	c.mu.Unlock()
	if gate != nil {
		close(arrived)
		<-gate
	}
	return c.Cache.GetSetMembers(ctx, key)
}

func (c *gatedCache) pause() (reached <-chan struct{}, release func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gate = make(chan struct{})
	c.arrived = make(chan struct{})
	gate := c.gate
	return c.arrived, func() { close(gate) }
}

func TestReadingAResultWhileAnotherRequestRefreshesIt(t *testing.T) {
	for _, withResolver := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale data is neither served nor stored (cache resolver: %v)", withResolver), func(t *testing.T) {
			cache := &gatedCache{Cache: memoryBackend().newCache(t, nil)}
			state := map[string]int{"1": 1, "2": 1}
			directive := ""
			if withResolver {
				directive = "@cacheResolver"
			}
			var mu sync.Mutex
			value := func(id string) int {
				mu.Lock()
				defer mu.Unlock()
				return state[id]
			}
			h := newHarness(t, harnessOptions{
				cache: cache,
				resolvers: testexec.Resolvers{"Query": {
					"item": fn(func(_ obj, args obj) any { return obj{"id": args["id"], "v": value(args["id"].(string))} }),
					"items": fn0(func() any {
						return list(obj{"id": "1", "v": value("1")}, obj{"id": "2", "v": value("2")})
					}),
				}},
				typeDefs: fmt.Sprintf(`
					type Query { items: [Item!]! item(id: ID!): Item %s }
					type Item { id: ID! v: Int! }`, directive),
			})
			query := `{ items { id v } }`

			h.run(query)
			mu.Lock()
			state["1"] = 2
			mu.Unlock()
			h.invalidate(mangrove.Entity("Item", "1"))

			// A reads the old result, then stalls before reading its
			// references.
			reached, release := cache.pause()
			a := h.runAsync(query)
			<-reached

			// B refreshes item 1 and stores; item 2 changes afterwards.
			h.expectFresh(query)
			mu.Lock()
			state["2"] = 2
			mu.Unlock()
			h.invalidate(mangrove.Entity("Item", "2"))

			release()
			// A started after item 1's invalidation completed.
			expectEqual(t, a.wait(t).path(t, "items", 0, "v"), 2.0)

			h.expectFresh(query)
			h.expectFresh(query)
		})
	}
}

func TestNewListMemberIsNotCompletedFromRemovedFields(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		libraries := []obj{
			{"best": "b1", "id": 1, "shelves": []string{"s1"}},
			{"best": "b2", "id": 2, "shelves": []string{"s2"}},
		}
		shelves := map[string]obj{
			"s1": {"id": "s1", "name": "s1"},
			"s2": {"id": "s2", "name": "s2"},
			"s3": {"id": "s3", "name": "s3"},
		}
		books := map[string]obj{"b1": {"id": "b1"}, "b2": {"id": "b2"}, "b3": {"id": "b3"}}
		h := newHarness(t, harnessOptions{
			cache: b.newCache(t, nil),
			resolvers: testexec.Resolvers{
				"Library": {
					"shelves": fn(func(l obj, _ obj) any {
						var out []any
						for _, id := range l["shelves"].([]string) {
							out = append(out, shelves[id])
						}
						return out
					}),
					"stats": fn(func(l obj, _ obj) any {
						return obj{"best": books[l["best"].(string)], "count": len(l["shelves"].([]string))}
					}),
				},
				"Query": {
					"book":      fn(func(_ obj, args obj) any { return books[args["id"].(string)] }),
					"libraries": fn0(func() any { return list(libraries...) }),
				},
			},
			typeDefs: `
				type Query { libraries: [Library!]! book(id: ID!): Book @cacheResolver }
				type Library { id: Int! stats: Stats! shelves: [Shelf!]! }
				type Stats { count: Int! best: Book }
				type Shelf { id: ID! name: String! }
				type Book { id: ID! }`,
		})
		query := `{ libraries { id stats { count best { id } } shelves { id name } } }`

		h.expectFresh(query)
		// Shelf s1 is refetched through the list, where library 3 turns up.
		// Its `stats` were left out of the query, as they hadn't changed for
		// the libraries in the cached result.
		libraries = append(libraries, obj{"best": "b3", "id": 3, "shelves": []string{"s3"}})
		shelves["s1"]["name"] = "changed"
		h.invalidate(mangrove.Entity("Shelf", "s1"))

		h.expectFresh(query)
		h.expectFresh(query)
	})
}
