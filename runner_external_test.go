package mangrove_test

import (
	"context"
	"testing"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// Ported from src/runner.test.ts. See runner_internal_test.go for why this is
// an external test.

func parseUnvalidated(t *testing.T, query string) *ast.QueryDocument {
	t.Helper()
	document, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, query)
	}
	return document
}

// parseSelectionSet mirrors @graphql-tools/utils parseSelectionSet.
func parseSelectionSet(t *testing.T, selectionSet string) ast.SelectionSet {
	t.Helper()
	return parseUnvalidated(t, selectionSet).Operations[0].SelectionSet
}

func TestLayeredCacheExecute(t *testing.T) {
	t.Run("handles multiple rounds of link query delegation", func(t *testing.T) {
		c := obj{"id": "3"}
		b := obj{"c": c, "id": "2"}
		a := obj{"b": b, "id": "1"}

		schema := testexec.MustSchema(`
        schema {
          query: Query
        }
        type A {
          id: ID
          b: B
        }
        type B {
          id: ID
          c: C
        }
        type C {
          id: ID
        }
        type Query {
          a(id: String): A
          b(id: String): B
          c(id: String): C
        }
      `, testexec.Resolvers{
			"Query": {
				"a": fn0(func() any { return a }),
				"b": fn0(func() any { return b }),
				"c": fn0(func() any { return c }),
			},
		})

		partialQuery := mangrove.DecorateClientDocument(parseUnvalidated(t, `
        query MyQuery {
          _ENTITY_someQuery__a_i0: a(id: "1") {
            id @idField
            b {
              __entityCacheTypeName: __typename
              __entityCacheId: id
            }
          }
        }
      `))

		resolvers := mangrove.CacheResolverMap{
			"A": {IDArg: "id", RootField: "a", Type: mangrove.KeyTypeString},
			"B": {IDArg: "id", RootField: "b", Type: mangrove.KeyTypeString},
			"C": {IDArg: "id", RootField: "c", Type: mangrove.KeyTypeString},
		}

		linkSelections := map[string]ast.SelectionSet{
			"Query.someQuery.a": parseSelectionSet(t, `
        {
          id
          __entityCacheTypeName: __typename
          __entityCacheId: id
          b {
            __entityCacheTypeName: __typename
            __entityCacheId: id
          }
        }
      `),
			"Query.someQuery.a.b": parseSelectionSet(t, `
        {
          id
          __entityCacheTypeName: __typename
          __entityCacheId: id
          c {
            __entityCacheTypeName: __typename
            __entityCacheId: id
          }
        }
      `),
			"Query.someQuery.a.b.c": parseSelectionSet(t, `
        {
          id
          __entityCacheTypeName: __typename
          __entityCacheId: id
        }
      `),
		}

		linkQueries, err := mangrove.LayeredCacheExecuteForTest(
			context.Background(),
			schema.Executor(),
			resolvers,
			linkSelections,
			"MyQuery",
			partialQuery,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(linkQueries) < 2 {
			t.Fatalf("expected 2 link queries, got %d", len(linkQueries))
		}

		expectEqual(t, mangrove.PrintDocument(linkQueries[0]), `query MyQuery__linkQuery {
  _ENTITY_someQuery__a__b_i0: b(id: "2") {
    id
    __entityCacheTypeName: __typename
    __entityCacheId: id
    c {
      __entityCacheTypeName: __typename
      __entityCacheId: id
    }
  }
}`)

		expectEqual(t, mangrove.PrintDocument(linkQueries[1]), `query MyQuery__linkQuery {
  _ENTITY_someQuery__a__b__c_i0: c(id: "3") {
    id
    __entityCacheTypeName: __typename
    __entityCacheId: id
  }
}`)
	})
}
