package mangrove_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
)

type itemStore struct {
	items  []map[string]any
	owners map[string]map[string]any
	calls  map[string]int
}

func newItemStore() *itemStore {
	return &itemStore{
		items: []map[string]any{
			{"id": "1", "name": "one", "owner": "o1"},
			{"id": "2", "name": "two", "owner": "o2"},
		},
		owners: map[string]map[string]any{
			"o1": {"id": "o1", "name": "Ann"},
			"o2": {"id": "o2", "name": "Bob"},
		},
		calls: map[string]int{},
	}
}

func (s *itemStore) count(ctx context.Context, name string) {
	if !isDirect(ctx) {
		s.calls[name]++
	}
}

func (s *itemStore) resolvers() testexec.Resolvers {
	return testexec.Resolvers{
		"Item": {
			"owner": func(_ context.Context, parent any, _ map[string]any) (any, error) {
				return s.owners[parent.(map[string]any)["owner"].(string)], nil
			},
		},
		"Query": {
			"items": func(ctx context.Context, _ any, _ map[string]any) (any, error) {
				s.count(ctx, "items")
				out := make([]any, len(s.items))
				for i, item := range s.items {
					out[i] = item
				}
				return out, nil
			},
			"item": func(ctx context.Context, _ any, args map[string]any) (any, error) {
				s.count(ctx, "item")
				for _, item := range s.items {
					if item["id"] == args["id"] {
						return item, nil
					}
				}
				return nil, nil
			},
			"owner": func(ctx context.Context, _ any, args map[string]any) (any, error) {
				s.count(ctx, "owner")
				if owner, ok := s.owners[args["id"].(string)]; ok {
					return owner, nil
				}
				return nil, nil
			},
		},
	}
}

const itemSchema = `
type Query {
  items: [Item!]!
  item(id: ID!): Item @cacheResolver
  owner(id: ID!): Owner @cacheResolver
}
type Item { id: ID! name: String! owner: Owner! }
type Owner { id: ID! name: String! }
`

func TestBasicFlow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, b backend) {
		store := newItemStore()
		h := newHarness(t, harnessOptions{cache: b.newCache(t, nil), resolvers: store.resolvers(), typeDefs: itemSchema})
		query := `{ items { id name owner { id name } } }`

		first := h.expectFresh(query)
		if first.hit() {
			t.Fatal("first request should miss")
		}
		second := h.expectFresh(query)
		if !second.hit() || second.partial() || len(second.executed) != 0 {
			t.Fatalf("second request should be a pure hit, executed %v", second.executed)
		}

		// One owner changes: only that owner is re-resolved.
		store.owners["o1"]["name"] = "Anna"
		h.invalidate(mangrove.Entity("Owner", "o1"))
		third := h.expectFresh(query)
		if !third.partial() || store.calls["items"] != 1 || store.calls["owner"] != 1 {
			t.Fatalf("expected a partial refresh through the owner resolver, calls %v, executed %v", store.calls, third.executed)
		}

		// A new list member is fetched through a link query.
		store.items = append(store.items, map[string]any{"id": "3", "name": "three", "owner": "o1"})
		h.invalidate(mangrove.EntityType("Query"))
		fourth := h.expectFresh(query)
		if store.calls["items"] != 2 || store.calls["item"] != 1 {
			t.Fatalf("expected the list and one link, calls %v, executed:\n%s", store.calls, strings.Join(fourth.executed, "\n"))
		}

		h.expectFresh(query)
		h.expectNoErrors()
		if warnings := h.logs.Warnings(); len(warnings) > 0 {
			t.Fatalf("unexpected warnings: %v", warnings)
		}
	})
}
