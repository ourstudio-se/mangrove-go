package gqlgen_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/gqlgen"
	"github.com/ourstudio-se/mangrove-go/gqlgen/internal/testserver"
	"github.com/ourstudio-se/mangrove-go/memorycache"
)

type server struct {
	t        *testing.T
	url      string
	store    *testserver.Store
	strategy *mangrove.LazyStrategy
	// fields counts resolved fields by name, through field middleware.
	fields   map[string]int
	fieldsMu sync.Mutex
}

type serverOptions struct {
	metadata bool
	// hideSecrets makes field middleware hide Item.secret unless the request
	// has the "X-Allowed: yes" header.
	hideSecrets bool
}

func newServer(t *testing.T, options serverOptions) *server {
	store := testserver.NewStore()
	strategy := mangrove.NewLazyStrategy(mangrove.LazyStrategyOptions{Cache: memorycache.New(memorycache.Options{})})
	s := &server{t: t, store: store, strategy: strategy, fields: map[string]int{}}

	srv := handler.New(testserver.NewExecutableSchema(testserver.Config{Resolvers: &testserver.Resolver{Store: store}}))
	srv.AddTransport(transport.POST{})
	srv.Use(extension.Introspection{})
	srv.AroundFields(func(ctx context.Context, next graphql.Resolver) (any, error) {
		field := graphql.GetFieldContext(ctx)
		s.fieldsMu.Lock()
		s.fields[field.Field.Name]++
		s.fieldsMu.Unlock()
		if options.hideSecrets && field.Field.Name == "secret" &&
			graphql.GetOperationContext(ctx).Headers.Get("X-Allowed") != "yes" {
			return nil, nil
		}
		return next(ctx)
	})
	srv.Use(gqlgen.New(mangrove.Options{
		Strategy:                 strategy,
		TTL:                      time.Minute,
		AwaitWriteBeforeResponse: true,
		IncludeExtensionMetadata: options.metadata,
		Session: func(ctx context.Context) string {
			return graphql.GetOperationContext(ctx).Headers.Get("X-Allowed")
		},
	}))

	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)
	s.url = httpServer.URL
	return s
}

type response struct {
	Data       json.RawMessage `json:"data"`
	Errors     []any           `json:"errors"`
	Extensions map[string]any  `json:"extensions"`
}

func (s *server) post(body map[string]any, headers ...string) response {
	s.t.Helper()
	encoded, _ := json.Marshal(body)
	request, _ := http.NewRequest(http.MethodPost, s.url, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	for i := 0; i < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	httpResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer httpResponse.Body.Close()
	var out response
	if err := json.NewDecoder(httpResponse.Body).Decode(&out); err != nil {
		s.t.Fatal(err)
	}
	return out
}

func (s *server) query(query string, headers ...string) response {
	s.t.Helper()
	return s.post(map[string]any{"query": query}, headers...)
}

func (s *server) invalidate(entities ...mangrove.EntityRecord) {
	s.t.Helper()
	if err := s.strategy.InvalidateEntities(context.Background(), entities); err != nil {
		s.t.Fatal(err)
	}
}

func expectJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	// Field order is part of the response.
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func expectEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestExtension(t *testing.T) {
	t.Run("caches, and partially re-executes after invalidation", func(t *testing.T) {
		s := newServer(t, serverOptions{})
		query := `{ items { id name owner { id name } } }`
		want := `{"items":[{"id":"1","name":"one","owner":{"id":"o1","name":"Ann"}},{"id":"2","name":"two","owner":{"id":"o2","name":"Bob"}}]}`

		first := s.query(query)
		expectJSON(t, first.Data, want)
		expectEqual(t, first.Extensions == nil, true)
		expectJSON(t, s.query(query).Data, want)
		expectEqual(t, s.store.Calls("items"), 1)

		s.store.SetOwnerName("o1", "Anna")
		s.invalidate(mangrove.Entity("Owner", "o1"))

		expectJSON(t, s.query(query).Data, strings.Replace(want, "Ann", "Anna", 1))
		// The owner was re-resolved through `owner(id:)`, not through the
		// list.
		expectEqual(t, s.store.Calls("items"), 1)
		expectEqual(t, s.store.Calls("owner"), 1)
	})

	t.Run("new list members are fetched through link queries", func(t *testing.T) {
		s := newServer(t, serverOptions{})
		query := `query Q($first: Int) { items(first: $first) { id name owner { id name } } }`
		body := map[string]any{"query": query, "variables": map[string]any{"first": 10}}

		s.post(body)
		s.store.AddItem("3", "three", "o1")
		s.invalidate(mangrove.EntityType("Query"))

		refreshed := s.post(body)
		expectEqual(t, len(refreshed.Errors), 0)
		expectJSON(t, refreshed.Data, `{"items":[{"id":"1","name":"one","owner":{"id":"o1","name":"Ann"}},{"id":"2","name":"two","owner":{"id":"o2","name":"Bob"}},{"id":"3","name":"three","owner":{"id":"o1","name":"Ann"}}]}`)
		expectEqual(t, s.store.Calls("items"), 2)
		expectEqual(t, s.store.Calls("item"), 1)
		// The owner of the new item is known from the cached result.
		expectEqual(t, s.store.Calls("owner"), 0)
	})

	t.Run("unions and interface resolvers", func(t *testing.T) {
		s := newServer(t, serverOptions{})
		query := `{ search { ... on Item { id name } ... on Owner { id name } } }`

		s.query(query)
		s.store.SetItemName("2", "deux")
		s.invalidate(mangrove.Entity("Item", "2"))

		refreshed := s.query(query)
		expectJSON(t, refreshed.Data, `{"search":[{"id":"1","name":"one"},{"id":"o1","name":"Ann"},{"id":"2","name":"deux"},{"id":"o2","name":"Bob"}]}`)
		// Item 2 was re-resolved through `item(id:)`, which takes precedence
		// over `node(id:)`.
		expectEqual(t, s.store.Calls("item"), 1)
		expectEqual(t, s.store.Calls("node"), 0)
	})

	t.Run("field middleware and sessions apply to generated documents", func(t *testing.T) {
		s := newServer(t, serverOptions{hideSecrets: true})
		s.store.Secret = "s3cret"
		query := `{ items { id name secret } }`

		expectJSON(t, s.query(query).Data, `{"items":[{"id":"1","name":"one","secret":null},{"id":"2","name":"two","secret":null}]}`)
		expectJSON(t, s.query(query, "X-Allowed", "yes").Data, `{"items":[{"id":"1","name":"one","secret":"s3cret"},{"id":"2","name":"two","secret":"s3cret"}]}`)

		s.store.SetItemName("1", "uno")
		s.invalidate(mangrove.Entity("Item", "1"))

		// Item 1 is re-resolved through `item(id:)` for both sessions; the
		// middleware still sees each request's headers.
		expectJSON(t, s.query(query).Data, `{"items":[{"id":"1","name":"uno","secret":null},{"id":"2","name":"two","secret":null}]}`)
		expectJSON(t, s.query(query, "X-Allowed", "yes").Data, `{"items":[{"id":"1","name":"uno","secret":"s3cret"},{"id":"2","name":"two","secret":"s3cret"}]}`)
		expectEqual(t, s.store.Calls("item"), 2)
	})

	t.Run("mutations run normally, without marker fields", func(t *testing.T) {
		s := newServer(t, serverOptions{})
		result := s.query(`
			fragment I on Item { id name }
			mutation { rename(id: "1", name: "x") { ...I } }`)
		expectJSON(t, result.Data, `{"rename":{"id":"1","name":"x"}}`)
	})

	t.Run("several operations in a document", func(t *testing.T) {
		s := newServer(t, serverOptions{})
		document := `
			fragment I on Item { id name }
			query Get { items { ...I } }
			mutation Rename { rename(id: "1", name: "uno") { ...I } }`

		expectJSON(t, s.post(map[string]any{"query": document, "operationName": "Get"}).Data, `{"items":[{"id":"1","name":"one"},{"id":"2","name":"two"}]}`)
		expectJSON(t, s.post(map[string]any{"query": document, "operationName": "Rename"}).Data, `{"rename":{"id":"1","name":"uno"}}`)
	})

	t.Run("introspection", func(t *testing.T) {
		s := newServer(t, serverOptions{})
		result := s.query(`{ __schema { queryType { name } } }`)
		expectEqual(t, len(result.Errors), 0)
		expectJSON(t, result.Data, `{"__schema":{"queryType":{"name":"Query"}}}`)
	})

	t.Run("metadata is only exposed when asked for", func(t *testing.T) {
		s := newServer(t, serverOptions{metadata: true})
		result := s.query(`{ items { id } }`)
		cache := result.Extensions["cache"].(map[string]any)
		expectEqual(t, cache["hit"], false)
		expectJSON(t, result.Data, `{"items":[{"id":"1"},{"id":"2"}]}`)
		cache = s.query(`{ items { id } }`).Extensions["cache"].(map[string]any)
		expectEqual(t, cache["hit"], true)
	})

	t.Run("invalid documents are rejected before Mangrove sees them", func(t *testing.T) {
		s := newServer(t, serverOptions{})
		result := s.query(`{ items { nope } }`)
		expectEqual(t, len(result.Errors), 1)
	})
}
