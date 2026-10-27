package mangrove_test

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	"github.com/vektah/gqlparser/v2/validator/rules"
)

func configure(t *testing.T, typeDefs string) (mangrove.SchemaConfig, *recordingHandler, *ast.Schema) {
	t.Helper()
	schema, err := gqlparser.LoadSchema(&ast.Source{Input: directives + typeDefs})
	if err != nil {
		t.Fatal(err)
	}
	logs := &recordingHandler{}
	return mangrove.ConfigureFromSchema(schema, []string{"id"}, slog.New(logs)), logs, schema
}

func parseQuery(t *testing.T, query string) *ast.QueryDocument {
	t.Helper()
	document, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestSchemaConfiguration(t *testing.T) {
	t.Run("key argument types", func(t *testing.T) {
		config, logs, _ := configure(t, `
			type Query {
				a(id: ID!): A @cacheResolver
				b(id: Float!): B @cacheResolver
				c(id: Long!): C @cacheResolver
				d(ids: [Int!]!): [D!]! @cacheResolver
			}
			scalar Long
			type A { id: ID! } type B { id: Float! } type C { id: Long! } type D { id: Int! }`)

		expectEqual(t, config.CacheResolvers["A"], mangrove.CacheResolver{
			IDArg: "id", RootField: "a", Type: mangrove.KeyTypeString, TypeConditions: []string{},
		})
		expectEqual(t, config.CacheResolvers["B"].Type, mangrove.KeyTypeFloat)
		if _, ok := config.CacheResolvers["C"]; ok {
			t.Fatal("C must have no resolver")
		}
		expectEqual(t, config.CacheResolvers["D"], mangrove.CacheResolver{
			Batch: true, IDArg: "ids", RootField: "d", Type: mangrove.KeyTypeInt, TypeConditions: []string{},
		})
		expectLen(t, logs.Warnings(), 1)
	})

	t.Run("rejects unusable root fields", func(t *testing.T) {
		config, logs, _ := configure(t, `
			type Query {
				a(id: ID!, locale: String!): A @cacheResolver
				b(wrong: ID!): B @cacheResolver(keyArg: "id")
				c(id: ID!, locale: String = "en"): C @cacheResolver
			}
			type A { id: ID! } type B { id: ID! } type C { id: ID! }`)

		if _, ok := config.CacheResolvers["A"]; ok {
			t.Fatal("A must have no resolver")
		}
		if _, ok := config.CacheResolvers["B"]; ok {
			t.Fatal("B must have no resolver")
		}
		expectEqual(t, config.CacheResolvers["C"].IDArg, "id")
		expectLen(t, logs.Warnings(), 2)
	})

	t.Run("abstract root fields resolve every possible type", func(t *testing.T) {
		config, _, _ := configure(t, `
			type Query { node(id: ID!): Node @cacheResolver article(id: ID!): Article @cacheResolver }
			interface Node { id: ID! }
			type Article implements Node { id: ID! }
			type Video implements Node { id: ID! }`)

		expectEqual(t, config.CacheResolvers["Video"].RootField, "node")
		expectEqual(t, config.CacheResolvers["Video"].Abstract, true)
		expectEqual(t, config.CacheResolvers["Video"].TypeConditions, []string{"Node"})
		// Concrete resolvers take precedence.
		expectEqual(t, config.CacheResolvers["Article"].RootField, "article")
		expectEqual(t, config.CacheResolvers["Article"].Abstract, false)
	})

	t.Run("entity TTLs and id fields", func(t *testing.T) {
		config, _, _ := configure(t, `
			type Query { a: A b: B }
			type A @cacheEntity(ttl: 5000) { id: ID! }
			type B { id(format: String!): ID! key: String }`)
		expectEqual(t, config.EntityTTLs["A"].Milliseconds(), int64(5000))
		expectEqual(t, config.IDFieldByTypename["A"], "id")
		// Ids must be selectable without arguments.
		if _, ok := config.IDFieldByTypename["B"]; ok {
			t.Fatal("B must have no id field")
		}
	})

	t.Run("the decorator identifies entities behind interfaces and unions", func(t *testing.T) {
		config, _, schema := configure(t, `
			type Query { node: Node u: U }
			interface Node { id: ID! }
			union U = A | B | V
			type A implements Node { id: ID! }
			type B implements Node { id: ID! }
			type V { value: Int }`)

		document := config.Decorate(parseQuery(t, `{ node { id } u { ... on V { value } } }`))
		printed := mangrove.PrintDocument(document)

		if errs := validator.ValidateWithRules(schema, parseQuery(t, printed), nil); len(errs) > 0 {
			t.Fatalf("invalid document: %v\n%s", errs, printed)
		}
		expectContains(t, printed, "node {\n    id\n    __entityCacheTypeName: __typename\n    __entityCacheId: id")
		expectContains(t, printed, "... on A {\n      __entityCacheId: id")
		expectContains(t, printed, "... on B {\n      __entityCacheId: id")
		expectNotContains(t, printed, "... on V {\n      __entityCacheId")
	})

	t.Run("typed id markers", func(t *testing.T) {
		config, _, _ := configure(t, `
			type Query { a: A b: B c: C }
			type A { id: ID } type B { id: Int! } type C { id: String }`)
		printed := mangrove.PrintDocument(config.Decorate(parseQuery(t, `{ a { id } b { id } c { id } }`)))
		expectContains(t, printed, "__entityCacheId_ID: id")
		expectContains(t, printed, "__entityCacheId_Int_: id")
		expectContains(t, printed, "__entityCacheId_String: id")
	})

	t.Run("decorating is idempotent (servers cache parsed documents)", func(t *testing.T) {
		config, _, _ := configure(t, `type Query { a: A } type A { id: ID! }`)
		once := config.Decorate(parseQuery(t, `{ a { id } }`))
		if config.Decorate(once) != once {
			t.Fatal("decorating twice changed the document")
		}
		client := mangrove.DecorateClientDocument(parseQuery(t, `{ a { id @idField } }`))
		if mangrove.DecorateClientDocument(client) != client {
			t.Fatal("decorating a client document twice changed it")
		}
	})

	t.Run("documents with mutations are left alone", func(t *testing.T) {
		config, _, _ := configure(t, `
			type Query { a: A } type Mutation { a: A } type A { id: ID! }`)
		document := parseQuery(t, `
			fragment F on A { id }
			mutation M { a { ...F } }
			query Q { a { ...F } }`)
		if config.Decorate(document) != document {
			t.Fatal("a document with a mutation was decorated")
		}
		expectNotContains(t, mangrove.PrintDocument(mangrove.DecorateClientDocument(document)), mangrove.AliasEntityCacheTypename)
	})

	t.Run("client documents", func(t *testing.T) {
		printed := mangrove.PrintDocument(mangrove.DecorateClientDocument(parseQuery(t, `{ a { key @idField name } b { x } }`)))
		expectNotContains(t, printed, "@idField")
		expectContains(t, printed, "__entityCacheId: key")
		if strings.Count(printed, "__entityCacheTypeName: __typename") != 3 {
			t.Fatalf("expected a typename marker in every selection set:\n%s", printed)
		}
	})
}

func TestNoAliasConventionConflictsRule(t *testing.T) {
	schema, err := gqlparser.LoadSchema(&ast.Source{Input: `type Query { a: Int b: B } type B { c: Int }`})
	if err != nil {
		t.Fatal(err)
	}
	validate := func(query string) int {
		ruleSet := rules.NewDefaultRules()
		ruleSet.AddRule(mangrove.NoAliasConventionConflictsRule.Name, mangrove.NoAliasConventionConflictsRule.RuleFunc)
		return len(validator.ValidateWithRules(schema, parseQuery(t, query), ruleSet))
	}

	expectEqual(t, validate(`{ a x: a b { c } }`), 0)
	// Reserved aliases are rejected, including in fragments.
	expectEqual(t, validate(`
		fragment F on Query { _ENTITY_x: a }
		{ ...F b { _ENTITY_y: c } }`), 2)
}
