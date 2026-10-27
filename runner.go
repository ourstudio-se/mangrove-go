package mangrove

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
)

// Options configure Mangrove.
type Options struct {
	// Strategy reads, stores and invalidates cached results.
	Strategy InvalidationStrategy
	// TTL is how long results are cached for.
	TTL time.Duration
	// EntityTTLs limit how long results are cached for, by the types of the
	// entities they contain. Types annotated with @cacheEntity(ttl:) in
	// Schema override these.
	EntityTTLs map[string]time.Duration
	// CacheResolvers are root query fields that resolve entities by id, by
	// type. Fields annotated with @cacheResolver in Schema override these.
	CacheResolvers CacheResolverMap
	// Schema, if set, is used to configure cache resolvers and entity TTLs
	// from directives, and to decorate documents with the marker fields
	// Mangrove needs. Without a schema, documents must be decorated by the
	// caller (see DecorateClientDocument).
	Schema *ast.Schema
	// IDFields are the names of id fields, in order of preference. Only used
	// with Schema. Defaults to "id".
	IDFields []string
	// Session returns an identifier that partitions the cache. Results are
	// only shared between requests that return the same identifier; "" means
	// the result is shared by all requests without a session.
	Session func(ctx context.Context) string
	// Enabled decides whether a request uses the cache. Defaults to always.
	Enabled func(ctx context.Context) bool
	// BuildCacheKey builds the cache key of a request.
	BuildCacheKey BuildCacheKeyFunc
	// GetDocumentString returns the document string used in the cache key.
	// Defaults to printing the (decorated) document.
	GetDocumentString func(request *Request) string
	// CollectEntity decides which objects in result data are entities.
	CollectEntity CollectEntityFunc
	// ShouldCacheResult decides whether a result is stored. Defaults to
	// results without errors.
	ShouldCacheResult ShouldCacheResultFunc
	// AwaitWriteBeforeResponse: store results before responding, instead of
	// in the background.
	AwaitWriteBeforeResponse bool
	// IncludeExtensionMetadata exposes cache metadata (cache key, known
	// entity ids, generated queries) in the response extensions. Meant for
	// debugging only.
	IncludeExtensionMetadata bool
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Mangrove caches GraphQL responses and refreshes them partially.
type Mangrove struct {
	strategy                 InvalidationStrategy
	ttl                      time.Duration
	cacheResolvers           CacheResolverMap
	session                  func(ctx context.Context) string
	enabled                  func(ctx context.Context) bool
	buildCacheKey            BuildCacheKeyFunc
	getDocumentString        func(request *Request) string
	includeExtensionMetadata bool
	logger                   *slog.Logger
	processor                *resultProcessor
	decorate                 func(*ast.QueryDocument) *ast.QueryDocument
	printed                  weakMemo[ast.QueryDocument, string]
}

// New creates a Mangrove instance.
func New(options Options) (*Mangrove, error) {
	if options.Strategy == nil {
		return nil, errors.New("mangrove: a Strategy is required")
	}
	if options.TTL <= 0 {
		return nil, errors.New("mangrove: TTL must be positive")
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}

	session := options.Session
	if session == nil {
		session = func(context.Context) string { return "" }
	}

	entityTTLs := map[string]time.Duration{}
	for typename, ttl := range options.EntityTTLs {
		entityTTLs[typename] = ttl
	}
	cacheResolvers := CacheResolverMap{}
	for typename, resolver := range options.CacheResolvers {
		cacheResolvers[typename] = resolver
	}

	m := &Mangrove{
		strategy:                 options.Strategy,
		ttl:                      options.TTL,
		session:                  session,
		enabled:                  options.Enabled,
		buildCacheKey:            options.BuildCacheKey,
		getDocumentString:        options.GetDocumentString,
		includeExtensionMetadata: options.IncludeExtensionMetadata,
		logger:                   logger,
	}

	if options.Schema != nil {
		idFields := options.IDFields
		if len(idFields) == 0 {
			idFields = []string{"id"}
		}
		config := ConfigureFromSchema(options.Schema, idFields, logger)
		// Configured values, overridden by what the schema declares.
		for typename, ttl := range config.EntityTTLs {
			entityTTLs[typename] = ttl
		}
		cacheResolvers = WithTypeConditions(options.Schema, cacheResolvers)
		for typename, resolver := range config.CacheResolvers {
			cacheResolvers[typename] = resolver
		}
		m.decorate = config.Decorate
	}

	m.cacheResolvers = cacheResolvers

	if m.buildCacheKey == nil {
		m.buildCacheKey = DefaultBuildCacheKey
	}
	if m.getDocumentString == nil {
		m.getDocumentString = func(request *Request) string {
			if request.DocumentString != "" {
				return request.DocumentString
			}
			return m.printed.get(request.Document, PrintDocument)
		}
	}

	shouldCacheResult := options.ShouldCacheResult
	if shouldCacheResult == nil {
		shouldCacheResult = DefaultShouldCacheResult
	}
	collectEntity := options.CollectEntity
	if collectEntity == nil {
		collectEntity = DefaultCollectEntity
	}

	m.processor = &resultProcessor{
		awaitWriteBeforeResponse: options.AwaitWriteBeforeResponse,
		collectEntity:            collectEntity,
		entityTTLs:               entityTTLs,
		logger:                   logger,
		shouldCacheResult:        shouldCacheResult,
		store:                    options.Strategy.StoreExecutionResult,
		ttl:                      options.TTL,
	}

	return m, nil
}

// CacheResolvers returns the cache resolvers in use.
func (m *Mangrove) CacheResolvers() CacheResolverMap {
	return m.cacheResolvers
}

// InvalidateEntities invalidates entities through the strategy.
func (m *Mangrove) InvalidateEntities(ctx context.Context, entities ...EntityRecord) error {
	return m.strategy.InvalidateEntities(ctx, entities)
}

// Decorate decorates a document with the marker fields Mangrove needs, if
// Mangrove was configured with a schema. Decorating is idempotent.
func (m *Mangrove) Decorate(document *ast.QueryDocument) *ast.QueryDocument {
	if m.decorate == nil {
		return document
	}
	return m.decorate(document)
}

// Wrap returns an executor that serves queries from the cache, executing
// what is needed through next.
func (m *Mangrove) Wrap(next Executor) Executor {
	return func(ctx context.Context, request *Request) (*Response, error) {
		return m.Execute(ctx, next, request)
	}
}

type entityCollectorKey struct{}

// WithEntityCollector returns a context that makes Mangrove report every
// entity a result depends on to collect (e.g. for live queries).
func WithEntityCollector(ctx context.Context, collect func(EntityRecord)) context.Context {
	return context.WithValue(ctx, entityCollectorKey{}, collect)
}

type runFunc func(ctx context.Context, document *ast.QueryDocument, operationName string) (*Response, error)

func bindExecutor(next Executor, request *Request) runFunc {
	// Persisted query extensions refer to the original document, not to the
	// documents generated by Mangrove.
	var extensions map[string]any
	if request.Extensions != nil {
		extensions = make(map[string]any, len(request.Extensions))
		for key, value := range request.Extensions {
			if key != "persistedQuery" {
				extensions[key] = value
			}
		}
	}
	return func(ctx context.Context, document *ast.QueryDocument, operationName string) (*Response, error) {
		return next(ctx, &Request{
			Document:      document,
			OperationName: operationName,
			Variables:     request.Variables,
			Extensions:    extensions,
		})
	}
}

var markerPrefix = []byte("__entityCache")

// stripMarkers makes sure that marker fields never reach the client.
func stripMarkers(response *Response) (*Response, error) {
	if len(response.Data) == 0 || !bytes.Contains(response.Data, markerPrefix) {
		return response, nil
	}
	data, err := DecodeJSON(response.Data)
	if err != nil {
		return nil, err
	}
	encoded, err := EncodeJSON(cleanupData(data))
	if err != nil {
		return nil, err
	}
	stripped := *response
	stripped.Data = encoded
	return &stripped, nil
}

func runDecoded(ctx context.Context, run runFunc, document *ast.QueryDocument, operationName string) (*executionResult, error) {
	response, err := run(ctx, document, operationName)
	if err != nil {
		return nil, err
	}
	return decodeResponse(response)
}

type layeredResult struct {
	linkQueries []*ast.QueryDocument
	resolutions []ResolutionExpectations
	results     []*executionResult
}

// layeredCacheExecute executes the partial query, then keeps executing link
// queries for every linked entity that the cache has no data for at that
// position, until there are none left.
func layeredCacheExecute(
	ctx context.Context,
	run runFunc,
	resolvers CacheResolverMap,
	linkSelections map[string]ast.SelectionSet,
	snapshots *entitySnapshots,
	originalOperationName string,
	partialQuery *ast.QueryDocument,
	resolutions ResolutionExpectations,
	variableDefinitions ast.VariableDefinitionList,
) (*layeredResult, error) {
	layered := &layeredResult{}

	if partialQuery == nil {
		return layered, nil
	}

	next, err := runDecoded(ctx, run, partialQuery, originalOperationName)
	if err != nil {
		return nil, err
	}

	layered.results = append(layered.results, next)
	layered.resolutions = append(layered.resolutions, resolutions)

	if next.data == nil || len(next.errors) > 0 {
		return layered, nil
	}

	linkCoordinates := make(map[string]bool, len(linkSelections))
	for coordinates := range linkSelections {
		linkCoordinates[coordinates] = true
	}
	collectLinks := makeLinkCollector(snapshots, linkCoordinates)

	for round := 0; ; round++ {
		links, err := collectLinks(next.data)
		if err != nil {
			return nil, err
		}

		if len(links) == 0 {
			break
		}

		if round >= maxLinkRounds {
			return nil, &CacheMergeError{Message: fmt.Sprintf("Exceeded %d rounds of link queries", maxLinkRounds)}
		}

		query, err := buildLinkQuery(links, linkSelections, resolvers, originalOperationName, variableDefinitions)
		if err != nil {
			return nil, err
		}

		if query == nil {
			break
		}

		next, err = runDecoded(ctx, run, query.document, query.operationName)
		if err != nil {
			return nil, err
		}

		layered.linkQueries = append(layered.linkQueries, query.document)
		layered.results = append(layered.results, next)
		layered.resolutions = append(layered.resolutions, query.resolutions)

		if next.data == nil || len(next.errors) > 0 {
			break
		}
	}

	return layered, nil
}

// Execute serves a request from the cache, executing what is needed through
// next.
func (m *Mangrove) Execute(ctx context.Context, next Executor, request *Request) (*Response, error) {
	operationName := request.OperationName
	operation := getOperation(request.Document, operationName)

	if (m.enabled != nil && !m.enabled(ctx)) ||
		operation == nil ||
		operation.Operation != ast.Query ||
		isIntrospectionDocument(request.Document) {
		// Documents may have been decorated with Mangrove's marker fields by
		// the caller; make sure they don't reach the client.
		response, err := bindExecutor(next, request)(ctx, request.Document, operationName)
		if err != nil {
			return nil, err
		}
		return stripMarkers(response)
	}

	if m.decorate != nil {
		decorated := *request
		decorated.Document = m.decorate(request.Document)
		request = &decorated
		operation = getOperation(request.Document, operationName)
	}

	run := bindExecutor(next, request)

	cacheKey, err := m.buildCacheKey(ctx, BuildCacheKeyParams{
		DocumentString: m.getDocumentString(request),
		OperationName:  operationName,
		SessionID:      m.session(ctx),
		Variables:      request.Variables,
	})
	if err != nil {
		return nil, err
	}

	opts, err := m.strategy.GetPartialExecutionOpts(ctx, GetPartialExecutionOptsParams{
		CacheKey:      cacheKey,
		OperationName: operationName,
		Query:         request.Document,
		Resolvers:     m.cacheResolvers,
	})
	if err != nil {
		m.logger.ErrorContext(ctx, "Unable to read from cache, falling back to uncached execution", "error", err)
		// Without the strategy's read state the result can't be stored
		// safely.
		response, err := run(ctx, request.Document, operationName)
		if err != nil {
			return nil, err
		}
		return stripMarkers(response)
	}

	cacheState := opts.CacheState
	query := opts.Query
	linkSelections := opts.LinkSelections
	cachedResult := opts.Result
	prunedCoordinates := opts.PrunedCoordinates
	resolutions := opts.Resolutions

	// Errors stored with a cached result can't be attributed to the parts of
	// it that would be refetched: such results are only served whole.
	if cachedResult != nil && len(cachedResult.Errors) > 0 && query != nil {
		cachedResult = nil
		query = request.Document
		linkSelections = nil
		prunedCoordinates = nil
		resolutions = nil
	}

	linkCoordinates := make([]string, 0, len(linkSelections))
	for coordinates := range linkSelections {
		linkCoordinates = append(linkCoordinates, coordinates)
	}

	var cachedData *Object
	if cachedResult != nil {
		cachedData = cachedResult.Data
	}
	mergeContext := newMergeContext(cachedData, prunedCoordinates, linkCoordinates)

	originalOperationName := operationName
	if originalOperationName == "" {
		originalOperationName = operation.Name
	}

	var linkQueries []*ast.QueryDocument
	usedCachedResult := cachedResult

	processed, err := func() (*processedResult, error) {
		layered, err := layeredCacheExecute(
			ctx,
			run,
			m.cacheResolvers,
			linkSelections,
			mergeContext.snapshots,
			originalOperationName,
			query,
			resolutions,
			operation.VariableDefinitions,
		)
		if err != nil {
			return nil, err
		}

		linkQueries = layered.linkQueries

		if cachedResult != nil {
			for _, result := range layered.results {
				if len(result.errors) > 0 {
					// Error paths refer to the generated documents and the
					// partial data can't be trusted; get the real thing
					// instead.
					return nil, &CacheMergeError{Message: "Partial execution returned errors"}
				}
			}
		}

		return m.processor.process(ctx, processResultParams{
			cacheKey:         cacheKey,
			cacheState:       cacheState,
			cachedResult:     cachedResult,
			mergeContext:     mergeContext,
			nextResults:      layered.results,
			resolutions:      layered.resolutions,
			originalDocument: request.Document,
		})
	}()

	if err != nil {
		if cachedResult == nil {
			return nil, err
		}

		m.logger.WarnContext(ctx, "Partial execution could not be completed, executing the full operation instead: "+err.Error())

		full, err := runDecoded(ctx, run, request.Document, operationName)
		if err != nil {
			return nil, err
		}

		query = request.Document
		linkQueries = nil
		usedCachedResult = nil

		processed, err = m.processor.process(ctx, processResultParams{
			cacheKey:         cacheKey,
			cacheState:       cacheState,
			nextResults:      []*executionResult{full},
			originalDocument: request.Document,
		})
		if err != nil {
			return nil, err
		}
	}

	if collect, ok := ctx.Value(entityCollectorKey{}).(func(EntityRecord)); ok {
		// Lets live query implementations know which entities a result
		// depends on.
		for _, record := range processed.collectedEntities {
			collect(record.Entity)
		}
	}

	return m.formatResult(cacheKey, usedCachedResult, processed, linkQueries, query)
}

func (m *Mangrove) formatResult(
	cacheKey string,
	cachedResult *CachedResult,
	processed *processedResult,
	linkQueries []*ast.QueryDocument,
	query *ast.QueryDocument,
) (*Response, error) {
	response := &Response{Errors: processed.errors, Extensions: processed.extensions}

	if processed.data != nil {
		encoded, err := EncodeJSON(processed.data)
		if err != nil {
			return nil, err
		}
		response.Data = encoded
	}

	if !m.includeExtensionMetadata {
		return response, nil
	}

	hit := cachedResult != nil
	known, typenames := knownEntities(processed.collectedEntities)
	knownObject := NewObject(len(typenames))
	for _, typename := range typenames {
		ids := make([]any, len(known[typename]))
		for i, id := range known[typename] {
			ids[i] = id
		}
		knownObject.Set(typename, ids)
	}

	metadata := map[string]any{
		"cacheKey":      cacheKey,
		"hit":           hit,
		"knownEntities": knownObject,
		"partial":       hit && query != nil,
	}
	if !hit {
		metadata["expires"] = time.Now().Add(m.ttl).UTC().Format(time.RFC3339Nano)
	}
	printed := make([]string, len(linkQueries))
	for i, linkQuery := range linkQueries {
		printed[i] = PrintDocument(linkQuery)
	}
	metadata["linkQueries"] = printed
	if hit && query != nil {
		metadata["partialQuery"] = PrintDocument(query)
	}

	extensions := make(map[string]any, len(response.Extensions)+1)
	for key, value := range response.Extensions {
		extensions[key] = value
	}
	extensions["cache"] = metadata
	response.Extensions = extensions

	return response, nil
}
