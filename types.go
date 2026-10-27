package mangrove

import (
	"context"
	"encoding/json"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// EntityRecord identifies an entity: an object of a type, with an id. An
// entity without an id stands for the type as a whole (or for the root
// object, for the root type).
type EntityRecord struct {
	Typename string
	ID       *string
}

// Entity identifies the entity of type typename with the given id.
func Entity(typename, id string) EntityRecord {
	return EntityRecord{Typename: typename, ID: &id}
}

// EntityType identifies every entity of a type (or the root object, for
// the root type) when invalidating.
func EntityType(typename string) EntityRecord {
	return EntityRecord{Typename: typename}
}

// PathPart is one step of a data path. Index is set for list items.
// Typename and ID describe the entity found at this position, if any, which
// tells the entity tree which ancestor entity a nested entity was reached
// through.
type PathPart struct {
	Field    string  `json:"field"`
	Index    *int    `json:"index,omitempty"`
	Typename string  `json:"typename,omitempty"`
	ID       *string `json:"id,omitempty"`
}

// EntityWithLocation is an entity found in result data, with its path.
type EntityWithLocation struct {
	Entity EntityRecord
	Path   []PathPart
}

type entityCacheResult struct {
	EntityWithLocation
	invalidated bool
}

// ResolverKeyType is the type of a cache resolver's key argument.
type ResolverKeyType string

const (
	KeyTypeString ResolverKeyType = "string"
	KeyTypeInt    ResolverKeyType = "int"
	KeyTypeFloat  ResolverKeyType = "float"
)

// CacheResolver describes a root query field that resolves an entity by its
// id, e.g. `item(id: ID!): Item`.
type CacheResolver struct {
	// Batch: the field takes a list of ids and returns a list.
	Batch     bool
	IDArg     string
	RootField string
	Type      ResolverKeyType
	// TypeConditions are the type conditions that apply to the resolved
	// type: its interfaces and the unions it belongs to. Fragments on
	// anything else are left out of resolutions. Nil means unknown; resolvers
	// configured from a schema have them filled in.
	TypeConditions []string
	// Abstract: the root field returns an interface or union, so resolutions
	// wrap their selections in a fragment on the resolved type.
	Abstract bool
}

// CacheResolverMap holds cache resolvers by the name of the type they
// resolve.
type CacheResolverMap map[string]CacheResolver

// Expectation is what a cache resolution requested.
type Expectation struct {
	IDs      []string
	Typename string
}

// ResolutionExpectations holds the entities each cache resolution alias
// requested, by alias.
type ResolutionExpectations map[string]Expectation

// CachedResult is a result as stored in the cache.
type CachedResult struct {
	Data   *Object
	Errors gqlerror.List
}

// Request is an operation to execute.
type Request struct {
	Document      *ast.QueryDocument
	OperationName string
	Variables     map[string]any
	// Extensions of the request, passed on to the executor. Mangrove drops
	// the persisted query extension for the documents it generates.
	Extensions map[string]any
}

// Response is the result of executing a request.
type Response struct {
	Data       json.RawMessage
	Errors     gqlerror.List
	Extensions map[string]any
}

// Executor executes GraphQL requests. Documents passed to an executor may
// share AST nodes with the original document and must not be modified.
type Executor func(ctx context.Context, request *Request) (*Response, error)

// PartialExecutionOpts tells the runner what to execute for a request.
type PartialExecutionOpts struct {
	// CacheState is opaque state produced by the invalidation strategy when
	// reading the cache, handed back to it when storing the result.
	CacheState any
	// LinkSelections are the selection sets for link queries, by
	// coordinates.
	LinkSelections map[string]ast.SelectionSet
	// PrunedCoordinates are the coordinates whose selection sets were
	// narrowed in Query.
	PrunedCoordinates []string
	// Resolutions are the entities requested by each cache resolution in
	// Query.
	Resolutions ResolutionExpectations
	// Query is the document to execute. Nil together with a Result means
	// that the cached result can be served as-is.
	Query *ast.QueryDocument
	// Result is the cached result, if any.
	Result *CachedResult
}

// GetPartialExecutionOptsParams are the parameters of
// InvalidationStrategy.GetPartialExecutionOpts.
type GetPartialExecutionOptsParams struct {
	CacheKey      string
	OperationName string
	Query         *ast.QueryDocument
	Resolvers     CacheResolverMap
}

// StoreParams are the parameters of InvalidationStrategy.StoreExecutionResult.
type StoreParams struct {
	CacheKey          string
	CacheState        any
	CollectedEntities []EntityWithLocation
	EntityTTLs        map[string]time.Duration
	Result            *CachedResult
	// MergedIntoCachedResult: the result was produced by merging fresh data
	// into a cached result, as opposed to executing the whole operation.
	MergedIntoCachedResult bool
	OriginalDocument       *ast.QueryDocument
	TTL                    time.Duration
}

// InvalidationStrategy reads and stores cached results, and invalidates
// entities.
type InvalidationStrategy interface {
	GetPartialExecutionOpts(ctx context.Context, params GetPartialExecutionOptsParams) (*PartialExecutionOpts, error)
	InvalidateEntities(ctx context.Context, entities []EntityRecord) error
	StoreExecutionResult(ctx context.Context, params StoreParams) error
}

// CollectEntityFunc decides whether an object in result data (found at
// path) is an entity. It returns the entity and its path, with the last part
// annotated with the entity's typename and id, or false.
type CollectEntityFunc func(data *Object, path []PathPart) (EntityWithLocation, bool)

// ShouldCacheResultFunc decides whether a result is stored.
type ShouldCacheResultFunc func(cacheKey string, result *CachedResult) bool

// BuildCacheKeyParams are the parameters of a BuildCacheKeyFunc.
type BuildCacheKeyParams struct {
	// DocumentString is the printed document.
	DocumentString string
	OperationName  string
	// SessionID partitions the cache; "" for results shared by every
	// request without a session.
	SessionID string
	Variables map[string]any
}

// BuildCacheKeyFunc builds the cache key of a request.
type BuildCacheKeyFunc func(ctx context.Context, params BuildCacheKeyParams) (string, error)

// LinkGroup is a set of linked entities of one type at one position.
type linkGroup struct {
	coordinates string
	ids         []string
	typename    string
}
