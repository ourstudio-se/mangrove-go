package mangrove

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// DefaultBuildCacheKey hashes the document, operation name, variables and
// session.
func DefaultBuildCacheKey(_ context.Context, params BuildCacheKeyParams) (string, error) {
	var operationName, sessionID any
	if params.OperationName != "" {
		operationName = params.OperationName
	}
	if params.SessionID != "" {
		sessionID = params.SessionID
	}
	variables := any(params.Variables)
	if params.Variables == nil {
		variables = map[string]any{}
	}
	// encoding/json sorts map keys, which makes the encoding stable.
	encoded, err := json.Marshal([]any{params.DocumentString, operationName, variables, sessionID})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// DefaultCollectEntity treats objects that carry both an entity typename and
// an id as entities. The root object is an entity identified by its
// typename alone, so that the root can be invalidated
// (`InvalidateEntities(ctx, []EntityRecord{EntityType("Query")})`).
//
// The returned path has its last part annotated with the entity's typename
// and id (normalized to a string).
func DefaultCollectEntity(data *Object, path []PathPart) (EntityWithLocation, bool) {
	typename, ok := getMarkedTypename(data)
	if !ok {
		return EntityWithLocation{}, false
	}

	id, hasID := getMarkedID(data)

	if !hasID {
		if len(path) == 1 {
			return EntityWithLocation{Entity: EntityRecord{Typename: typename}, Path: path}, true
		}
		return EntityWithLocation{}, false
	}

	annotated := make([]PathPart, len(path))
	copy(annotated, path)
	last := annotated[len(annotated)-1]
	last.ID = &id
	last.Typename = typename
	annotated[len(annotated)-1] = last

	return EntityWithLocation{Entity: EntityRecord{ID: &id, Typename: typename}, Path: annotated}, true
}

// DefaultShouldCacheResult caches results without errors.
func DefaultShouldCacheResult(_ string, result *CachedResult) bool {
	return len(result.Errors) == 0
}

/*
 * Internal keys start with ":", which no GraphQL type name (and thus no
 * default entity key) can start with.
 */

// DefaultBuildResultKey is the key of a cached result.
func DefaultBuildResultKey(cacheKey string) string {
	return ":result:" + cacheKey
}

// DefaultBuildOperationKey is the key of a cached operation's entity
// references.
func DefaultBuildOperationKey(cacheKey string) string {
	return ":operation:" + cacheKey
}

// DefaultBuildEntityKey is the key of an entity's set.
func DefaultBuildEntityKey(entity EntityRecord) string {
	key := entity.Typename
	if entity.ID != nil {
		key += ":" + *entity.ID
	}
	return key
}

// DefaultBuildTypeIndexKey is the key of a type's index of entity keys.
func DefaultBuildTypeIndexKey(typename string) string {
	return ":typeindex:" + typename
}
