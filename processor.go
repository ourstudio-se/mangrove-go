package mangrove

import (
	"context"
	"log/slog"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// executionResult is a decoded response.
type executionResult struct {
	// data is nil if the response had no data (or null).
	data       *Object
	errors     gqlerror.List
	extensions map[string]any
}

func decodeResponse(response *Response) (*executionResult, error) {
	result := &executionResult{errors: response.Errors, extensions: response.Extensions}
	if len(response.Data) == 0 {
		return result, nil
	}
	value, err := DecodeJSON(response.Data)
	if err != nil {
		return nil, err
	}
	if value != nil {
		object, ok := value.(*Object)
		if !ok {
			return nil, &CacheMergeError{Message: "response data is not an object"}
		}
		result.data = object
	}
	return result, nil
}

type processResultParams struct {
	cacheKey     string
	cacheState   any
	cachedResult *CachedResult
	// mergeContext is the merge state for cachedResult; created from it if
	// nil.
	mergeContext *mergeContext
	nextResults  []*executionResult
	// resolutions holds, for each of nextResults, the entities its cache
	// resolutions requested.
	resolutions      []ResolutionExpectations
	originalDocument *ast.QueryDocument
}

type processedResult struct {
	collectedEntities []EntityWithLocation
	// data is nil for a result without data.
	data       *Object
	errors     gqlerror.List
	extensions map[string]any
}

type resultProcessor struct {
	awaitWriteBeforeResponse bool
	collectEntity            CollectEntityFunc
	entityTTLs               map[string]time.Duration
	logger                   *slog.Logger
	shouldCacheResult        ShouldCacheResultFunc
	store                    func(context.Context, StoreParams) error
	ttl                      time.Duration
}

func (p *resultProcessor) process(ctx context.Context, params processResultParams) (*processedResult, error) {
	mergeContext := params.mergeContext
	if mergeContext == nil {
		var cachedData *Object
		if params.cachedResult != nil {
			cachedData = params.cachedResult.Data
		}
		mergeContext = newMergeContext(cachedData, nil, nil)
	}

	// Nothing to merge into: a plain execution result is used as is.
	isPlainExecution := params.cachedResult == nil && len(params.nextResults) == 1

	var mergedData *Object
	if isPlainExecution {
		mergedData = params.nextResults[0].data
	} else if params.cachedResult != nil && params.cachedResult.Data != nil {
		mergedData = cloneObject(params.cachedResult.Data)
	} else {
		mergedData = NewObject(0)
	}

	// Errors stored with the cached result (if results with errors are
	// cached at all) still apply to the parts of it that weren't refetched.
	var mergedErrors gqlerror.List
	if params.cachedResult != nil {
		mergedErrors = append(mergedErrors, params.cachedResult.Errors...)
	}
	var extensions map[string]any

	for index, next := range params.nextResults {
		if next.data != nil && !isPlainExecution {
			var expectations ResolutionExpectations
			if index < len(params.resolutions) {
				expectations = params.resolutions[index]
			}
			merged, err := mergeExecutionData(mergedData, next.data, mergeContext, expectations)
			if err != nil {
				return nil, err
			}
			mergedData = merged
		}

		mergedErrors = append(mergedErrors, next.errors...)

		if extensions == nil {
			extensions = next.extensions
		}
	}

	stored := &CachedResult{Data: mergedData, Errors: mergedErrors}

	var collected []EntityWithLocation
	if mergedData != nil {
		collected = collectEntityRecords(mergedData, p.collectEntity)
	}

	// A pure cache hit has nothing new to store. A result without data has
	// nothing to store either.
	isPureHit := params.cachedResult != nil && len(params.nextResults) == 0

	if !isPureHit && mergedData != nil && p.shouldCacheResult(params.cacheKey, stored) {
		storeParams := StoreParams{
			CacheKey:               params.cacheKey,
			CacheState:             params.cacheState,
			CollectedEntities:      collected,
			EntityTTLs:             p.entityTTLs,
			Result:                 stored,
			MergedIntoCachedResult: params.cachedResult != nil,
			OriginalDocument:       params.originalDocument,
			TTL:                    p.ttl,
		}
		store := func(ctx context.Context) {
			if err := p.store(ctx, storeParams); err != nil {
				p.logger.ErrorContext(ctx, "Unexpected error occured when storing execution result to cache. Result might not have been stored", "error", err)
			}
		}

		if p.awaitWriteBeforeResponse {
			store(ctx)
		} else {
			go store(context.WithoutCancel(ctx))
		}
	}

	var data *Object
	if mergedData != nil {
		data = cleanupData(mergedData).(*Object)
	}

	return &processedResult{
		collectedEntities: collected,
		data:              data,
		errors:            mergedErrors,
		extensions:        extensions,
	}, nil
}
