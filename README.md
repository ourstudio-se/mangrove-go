# Mangrove for Go

![Thanks, Dall-E, very cool](./assets/treeoflife2.jpg)

Mangrove is an adaptive entity tree cache for GraphQL. It caches whole responses, lets you invalidate individual entities (type + id combinations), and rewrites queries to only request the data that has become stale, in the most efficient way it can.

This is the Go port of [mangrove-graphql](https://github.com/ourstudio-se/mangrove). It comes with a [gqlgen](https://gqlgen.com) integration, but the core works with any executor.

## Installation

```bash
go get github.com/ourstudio-se/mangrove-go
```

## Why Mangrove?

Say a schema returns a list of items, and each item has a `data` field that is expensive to resolve (half a second, say):

```graphql
type Query {
  items: [Item!]!
  item(id: ID!): Item @cacheResolver
}

type Item {
  id: ID!
  data: String!
}
```

```graphql
query BigItemQuery {
  items {
    id
    data
  }
}
```

### Response caching

With Mangrove, the first execution goes as usual. Once it has happened, the response is cached and the same query returns the same answer, without resolving anything.

```jsonc
/* T1 */ [ { "id": "1", "data": "Whale" }, { "id": "2", "data": "Guanaco" } ]
/* T2 */ [ { "id": "1", "data": "Whale" }, { "id": "2", "data": "Guanaco" } ]
```

| | | |
|:-------------------------:|:-------------------------:|:-------------------------:|
|T1, T2|![Whale](./assets/whale.jpg)<br>ID=1 |![Guanaco](./assets/guanaco.jpg)<br>ID=2|

### Adaptive entity invalidations

What sets Mangrove apart is what happens when an entity in the response is invalidated. Let's invalidate the whale (the `Item` with id `"1"`):

```go
err := strategy.InvalidateEntities(ctx, []mangrove.EntityRecord{mangrove.Entity("Item", "1")})
```

Running the query again, only the whale is refetched (through the `item(id:)` cache resolver), while the guanaco stays cached:

```jsonc
/* T3 */ [ { "id": "1", "data": "Tortoise" }, { "id": "2", "data": "Guanaco" } ]
```

| | | |
|:-------------------------:|:-------------------------:|:-------------------------:|
|T3|![Tortoise](./assets/tortoise.jpg)<br>ID=1 |![Guanaco](./assets/guanaco.jpg)<br>ID=2|

### Linking

When a new item is added to the list, invalidate the root of the query:

```go
err := strategy.InvalidateEntities(ctx, []mangrove.EntityRecord{mangrove.EntityType("Query")})
```

Mangrove refetches the list, but only resolves `data` for the new item; the items it already knows are filled in from the cache:

```jsonc
/* T4 */ [
  { "id": "1", "data": "Tortoise" },
  { "id": "2", "data": "Guanaco" },
  { "id": "3", "data": "Goose" }
]
```

| | | | |
|:-------------------------:|:-------------------------:|:-------------------------:|:-------------------------:|
|T4|![Tortoise](./assets/tortoise.jpg)<br>ID=1 |![Guanaco](./assets/guanaco.jpg)<br>ID=2|![Goose](./assets/goose.jpg)<br>ID=3|

Reordering the list and invalidating the root again doesn't resolve `data` at all.

## How does it work?

Mangrove only ever caches entire GraphQL responses. It doesn't wrap resolvers or cache individual fields. What it does is rewrite the query based on the state of the cache.

- **Pruning.** From the cached result, Mangrove builds a tree of the entities it contains (objects with a typename and an id) and checks which ones are still valid. The query is pruned down to the branches that lead to invalidated entities. Unchanged entities are link points: had anything beyond them changed, they would have been invalidated themselves.
- **Cache resolutions.** Entities that have a cache resolver (a root field like `item(id:)`) are fetched through that shortcut instead of through their parents, under an alias that encodes where the data belongs (`_ENTITY_items_i0: item(id: "1") { … }`). An invalidated entity without a cache resolver is reached through its closest ancestor that has one, with the selection narrowed down to the path.
- **Links.** Linkable positions are replaced with links that only select the entity's typename and id. Entities that turn up at a link and aren't in the cached result at that position are fetched in follow-up link queries, through their cache resolvers, round by round.
- **Merging.** Fresh data is merged into the cached result by entity identity, then stored again.

## Usage

### With gqlgen

Declare Mangrove's directives in your schema, and tell gqlgen not to generate runtime code for them:

```graphql
directive @cacheResolver(keyArg: String) on FIELD_DEFINITION
directive @cacheEntity(ttl: Int) on OBJECT

type Query {
  items: [Item!]!
  item(id: ID!): Item @cacheResolver
}

type Item @cacheEntity(ttl: 300000) {
  id: ID!
  data: String!
}
```

```yaml
# gqlgen.yml
directives:
  cacheResolver:
    skip_runtime: true
  cacheEntity:
    skip_runtime: true
```

Then add the extension to your server:

```go
import (
	"github.com/ourstudio-se/mangrove-go"
	mangrovegql "github.com/ourstudio-se/mangrove-go/gqlgen"
	"github.com/ourstudio-se/mangrove-go/rediscache"
)

strategy := mangrove.NewLazyStrategy(mangrove.LazyStrategyOptions{
	Cache: rediscache.New(rediscache.Options{Client: redis.NewClient(&redis.Options{})}),
})

srv := handler.New(generated.NewExecutableSchema(config))
srv.AddTransport(transport.POST{})
srv.Use(mangrovegql.New(mangrove.Options{
	Strategy: strategy,
	TTL:      5 * time.Minute,
	// Partitions the cache: results are only shared between requests with
	// the same session id. "" means "shared by everyone".
	Session: func(ctx context.Context) string { return userID(ctx) },
}))
```

The extension intercepts queries. Mangrove's generated documents (partial queries, cache resolutions and link queries) run through the rest of the server's operation middleware and field middleware, like any other operation. Mutations, subscriptions, introspection and operations using `@defer` or `@stream` are executed as usual.

Invalidate entities through the strategy:

```go
strategy.InvalidateEntities(ctx, []mangrove.EntityRecord{
	mangrove.Entity("Item", "1"), // a single entity
	mangrove.EntityType("Item"),  // every entity of a type (and the type itself)
	mangrove.EntityType("Query"), // the root of every query, e.g. when a root list changes
})
```

Invalidate an entity whenever one of its fields changes, including fields that refer to other entities or lists of them. Changes to fields of the root `Query` type invalidate `Query`.

To reject client aliases that could be mistaken for Mangrove's, add `mangrove.NoAliasConventionConflictsRule` to the server's validation rules.

### With any executor

The core doesn't depend on a particular GraphQL server. `Mangrove.Wrap` wraps an executor:

```go
type Executor func(ctx context.Context, request *mangrove.Request) (*mangrove.Response, error)
```

```go
m, err := mangrove.New(mangrove.Options{
	Strategy: strategy,
	TTL:      5 * time.Minute,
	// With a schema, Mangrove reads @cacheResolver and @cacheEntity, and
	// decorates documents with the marker fields it needs.
	Schema: schema,
})
execute := m.Wrap(myExecutor)
response, err := execute(ctx, &mangrove.Request{Document: document, Variables: variables})
```

Mangrove executes the documents it generates through the wrapped executor. They are valid against the schema, and an executor must validate them (as any server does) so that field definitions are resolved.

### On the client

Mangrove doesn't need the schema at runtime, so it can just as well cache the responses of a remote API. Without a schema, cache resolvers are configured by hand, and documents are decorated with `DecorateClientDocument`, using the `@idField` directive to mark id fields:

```go
m, _ := mangrove.New(mangrove.Options{
	Strategy: strategy,
	TTL:      5 * time.Minute,
	CacheResolvers: mangrove.CacheResolverMap{
		"Launch": {IDArg: "id", RootField: "launch", Type: mangrove.KeyTypeString},
		"Rocket": {IDArg: "id", RootField: "rocket", Type: mangrove.KeyTypeString},
	},
})
execute := m.Wrap(httpexecutor.New("https://example.com/graphql", httpexecutor.Options{}))

parsed, _ := parser.ParseQuery(&ast.Source{Input: `
	query Launches {
		launches { id @idField details rocket { id @idField name } }
	}`})
response, err := execute(ctx, &mangrove.Request{Document: mangrove.DecorateClientDocument(parsed)})
```

Without a schema, `DecorateClientDocument` can't tell the types of id fields apart: if members of a union or implementations of an interface have id fields of different types (e.g. `ID!` and `ID`), mark only one of them with `@idField`, or configure Mangrove with the schema instead.

## Configuration

### Cache resolvers

A cache resolver is a root field that resolves an entity by its id. Annotate it in the schema:

```graphql
type Query {
  item(id: ID!): Item @cacheResolver
  # The key argument, if the field has more than one:
  product(sku: String!, locale: String = "en"): Product @cacheResolver(keyArg: "sku")
  # Batch resolvers take a list of ids:
  items(ids: [ID!]!): [Item!]! @cacheResolver
  # Abstract root fields resolve every possible type:
  node(id: ID!): Node @cacheResolver
}
```

or configure it by hand, which is equivalent to the first one:

```go
mangrove.Options{
	CacheResolvers: mangrove.CacheResolverMap{
		"Item": {IDArg: "id", RootField: "item", Type: mangrove.KeyTypeString},
	},
}
```

Key arguments can be `ID`, `String`, `Int` or `Float`. Resolvers declared for a concrete type take precedence over abstract ones.

### Entity TTLs

Every entity of a type can be given a shorter time to live than the result it is part of. When it runs out, only that entity is refetched. Use `@cacheEntity(ttl: <ms>)` on the type, or:

```go
mangrove.Options{EntityTTLs: map[string]time.Duration{"Item": 5 * time.Minute}}
```

This needs a cache that supports per-member expiry, which the Redis cache does unless `PlainSets` is set.

### Caches

```go
cache := rediscache.New(rediscache.Options{
	Client: client,
	// Prefix for every key written by Mangrove.
	KeyPrefix: "mangrove:",
	// How long invalidations are remembered, to reject results of requests
	// that started before them. Must exceed your longest request.
	InvalidationWindow: 10 * time.Minute,
	// Optional compression of stored results.
	Compress:   compress,
	Decompress: decompress,
})
```

`memorycache.New(memorycache.Options{})` has the same semantics, in process memory. It's useful for tests and single process deployments.

The Redis cache uses Lua scripts that touch several keys at once, so it doesn't support Redis Cluster. Expiry times are computed from the application servers' clocks, so keep them in sync with each other. Internal keys start with `:` (after the `KeyPrefix`), which no GraphQL type name can start with, so they never collide with entity keys such as `Item:1`.

### Other options

See `mangrove.Options`: `Enabled` (skip the cache for some requests), `BuildCacheKey`, `CollectEntity` (what counts as an entity), `ShouldCacheResult` (by default, results with errors aren't cached), `AwaitWriteBeforeResponse` (store before responding instead of in the background), `IncludeExtensionMetadata` (expose cache metadata in the response extensions, for debugging), `IDFields` and `Logger`. `WithEntityCollector` reports the entities a result depends on, e.g. for live queries.

## Consistency

Mangrove assumes that every change is announced by invalidating the entity whose fields changed, after the change has been committed. Under that assumption, a result served by Mangrove reflects every invalidation that completed before the request started (provided requests take less than `InvalidationWindow`), and no part of it is older than `TTL`. In more detail:

- **Invalidations during a request.** Every invalidation bumps an epoch. A request reads the epoch before it reads anything else, and when it stores its result it doesn't mark any entity as valid that has been invalidated since. The next request refetches it.
- **Concurrent requests.** A request never overwrites a result stored by a request that started after it.
- **Versioned results.** Entity sets vouch for a specific stored version of a result. A request that keeps cached data for an entity can only carry that entity's validity over from the version it read, if it is still valid, and never beyond its original expiry. Expiring entities, keys deleted directly, and concurrent writers therefore can't make stale data look valid.
- **Expiry.** A result keeps the expiry it was first stored with, even when parts of it are refreshed, so `TTL` bounds how old any part of a result can be. Cache hits never write to the cache.
- **When in doubt, execute.** If a partial execution returns errors, a cache resolver doesn't return every entity it was asked for, a link can't be resolved, or the cached data turns out to be inconsistent with the fresh data, Mangrove executes the original query instead. Errors are therefore always reported against the client's query.
- **Entity TTLs.** Refreshing part of a result never extends how long the entities that weren't refetched are considered valid.

### Caveats

- **Cache resolvers bypass parent resolvers.** An entity reached through a cache resolver is fetched without going through the path in the original query. Any authorization or filtering done by parent resolvers on that path is skipped; make sure cache resolvers apply the same rules. (Field middleware does run for generated documents.)
- **Only queries are cached.** Mutations and subscriptions are executed as usual. Documents that contain a mutation or subscription are not decorated at all.
- **Sessions.** Anything that changes the result of a query besides its document and variables (the user, their roles, the locale, ...) must be part of the session id.

## Performance

`BenchmarkScenarios` runs a query for 200 items (each with an owner and three tags, with cache resolvers for items and owners) against a local Redis, with the result written before responding. Per request, on a laptop:

| | TS 0.2 | Go |
|---|---|---|
| Plain execution, no cache | 1.3 ms | 2.1 ms (test executor) |
| Cache miss | 18 ms | 18 ms |
| Cache hit | 7 ms | 5 ms |
| Partial refresh (one item, one owner, or list reordered) | 18–19 ms | 18–19 ms |

```bash
nice go test -run '^$' -bench . -benchtime 100x
```

## Differences from the TS version

The port follows the TS version's behaviour (its tests and fuzzers have been ported along with it), with these differences:

- No compatibility with the TS version's stored data.
- Results with `data: null` keep it, instead of becoming `{}`.
- Sessions are strings; `""` means no session.
- TTLs are `time.Duration`s; `@cacheEntity(ttl:)` is in milliseconds, as before.
- The gqlgen extension replaces the Envelop plugin. Decorated documents are parsed and validated by the extension, and generated documents run through the server's middleware.
- Cache reads are batched explicitly (`GetWithEpoch`, `AreSetMembers`) instead of through a DataLoader.

## Development

```bash
go test ./...          # unit, regression, integration and differential tests
go test -race ./...
go vet ./...
```

The tests run against both the memory cache and a real Redis: either `REDIS_URL`, or a `redis-server` binary on the `PATH` (started on a random port). The differential tests compare random sequences of changes against direct execution; scale them up with e.g. `FUZZ_SEEDS=500 FUZZ_STEPS=250 nice go test -p 1 -run Differential .`.

The gqlgen test server is generated with `go tool gqlgen generate` in `gqlgen/internal/testserver`.

## Maintainer

- Max Bolotin max@ourstudio.se

## License

MIT
