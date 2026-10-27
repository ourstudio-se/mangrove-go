package gqlgen_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/gqlgen/internal/testserver"
	"github.com/ourstudio-se/mangrove-go/httpexecutor"
	"github.com/ourstudio-se/mangrove-go/memorycache"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// Mangrove on the client side: wrapping an HTTP executor in front of a plain
// gqlgen server, which validates every document Mangrove sends it.
func TestWrappingAnHTTPExecutor(t *testing.T) {
	store := testserver.NewStore()
	srv := handler.New(testserver.NewExecutableSchema(testserver.Config{Resolvers: &testserver.Resolver{Store: store}}))
	srv.AddTransport(transport.POST{})
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)

	strategy := mangrove.NewLazyStrategy(mangrove.LazyStrategyOptions{Cache: memorycache.New(memorycache.Options{})})
	m, err := mangrove.New(mangrove.Options{
		Strategy: strategy,
		TTL:      time.Minute,
		CacheResolvers: mangrove.CacheResolverMap{
			"Item":  {IDArg: "id", RootField: "item", Type: mangrove.KeyTypeString},
			"Owner": {IDArg: "id", RootField: "owner", Type: mangrove.KeyTypeString},
		},
		AwaitWriteBeforeResponse: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	execute := m.Wrap(httpexecutor.New(httpServer.URL, httpexecutor.Options{}))

	parsed, err := parser.ParseQuery(&ast.Source{Input: `
		query Q($first: Int) {
			items(first: $first) {
				id @idField
				name
				owner { id @idField name }
			}
		}`})
	if err != nil {
		t.Fatal(err)
	}
	document := mangrove.DecorateClientDocument(parsed)
	run := func() *mangrove.Response {
		t.Helper()
		response, err := execute(context.Background(), &mangrove.Request{Document: document, Variables: map[string]any{"first": 10}})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Errors) > 0 {
			t.Fatalf("unexpected errors: %v", response.Errors)
		}
		return response
	}

	first := run()
	expectJSON(t, first.Data, `{"items":[{"id":"1","name":"one","owner":{"id":"o1","name":"Ann"}},{"id":"2","name":"two","owner":{"id":"o2","name":"Bob"}}]}`)
	expectJSON(t, run().Data, string(first.Data))
	expectEqual(t, store.Calls("items"), 1)

	// Re-resolve one entity through its cache resolver.
	store.SetItemName("2", "deux")
	if err := strategy.InvalidateEntities(context.Background(), []mangrove.EntityRecord{mangrove.Entity("Item", "2")}); err != nil {
		t.Fatal(err)
	}
	expectJSON(t, run().Data, `{"items":[{"id":"1","name":"one","owner":{"id":"o1","name":"Ann"}},{"id":"2","name":"deux","owner":{"id":"o2","name":"Bob"}}]}`)
	expectEqual(t, store.Calls("items"), 1)
	expectEqual(t, store.Calls("item"), 1)

	// A new list member is fetched through a link query, known ones aren't.
	store.AddItem("3", "three", "o1")
	if err := strategy.InvalidateEntities(context.Background(), []mangrove.EntityRecord{mangrove.EntityType("Query")}); err != nil {
		t.Fatal(err)
	}
	expectJSON(t, run().Data, `{"items":[{"id":"1","name":"one","owner":{"id":"o1","name":"Ann"}},{"id":"2","name":"deux","owner":{"id":"o2","name":"Bob"}},{"id":"3","name":"three","owner":{"id":"o1","name":"Ann"}}]}`)
	expectEqual(t, store.Calls("items"), 2)
	expectEqual(t, store.Calls("item"), 2)
}
