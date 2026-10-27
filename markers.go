package mangrove

import (
	"encoding/json"
	"strings"
)

const (
	// AliasEntityCacheID is the alias of an entity's id marker field.
	AliasEntityCacheID = "__entityCacheId"
	// AliasEntityCacheTypename is the alias of an entity's typename marker
	// field.
	AliasEntityCacheTypename = "__entityCacheTypeName"

	// PartialCacheAliasPrefix starts the aliases of cache resolutions.
	PartialCacheAliasPrefix = "_ENTITY_"

	// DirectiveIDField marks the id field of an entity in client documents.
	DirectiveIDField = "idField"
	// DirectiveCacheResolver marks root query fields that resolve an entity
	// by its id.
	DirectiveCacheResolver = "cacheResolver"
	// DirectiveCacheEntity sets the TTL (in ms) of an entity type.
	DirectiveCacheEntity = "cacheEntity"

	// maxLinkRounds bounds the rounds of link queries for a single request.
	maxLinkRounds = 32

	rootQuery = "Query"
)

var coordinateRoot = map[string]string{
	"query":        "Query",
	"mutation":     "Mutation",
	"subscription": "Subscription",
}

/*
 * Mangrove adds "marker" fields to documents to identify entities:
 * `__entityCacheTypeName: __typename` and an id marker. The id marker of an
 * id field of type `ID!` is `__entityCacheId`; id fields of other types get a
 * suffix encoding the type (e.g. `__entityCacheId_ID` for a nullable ID), so
 * that the markers of differently typed ids (e.g. in different members of a
 * union) never conflict.
 */

const idMarkerPrefix = AliasEntityCacheID + "_"

func idMarkerAlias(idFieldType string) string {
	if idFieldType == "" || idFieldType == "ID!" {
		return AliasEntityCacheID
	}
	return idMarkerPrefix + strings.ReplaceAll(idFieldType, "!", "_")
}

func isIDMarkerKey(key string) bool {
	return key == AliasEntityCacheID || strings.HasPrefix(key, idMarkerPrefix)
}

func isMarkerKey(key string) bool {
	return key == AliasEntityCacheTypename || isIDMarkerKey(key)
}

// getMarkedID returns the entity id carried by an object's id markers, as a
// string.
func getMarkedID(value *Object) (string, bool) {
	for _, f := range value.fields {
		if !isIDMarkerKey(f.key) {
			continue
		}
		switch id := f.value.(type) {
		case string:
			return id, true
		case json.Number:
			return string(id), true
		}
	}
	return "", false
}

func getMarkedTypename(value *Object) (string, bool) {
	typename, ok := value.value(AliasEntityCacheTypename).(string)
	if !ok || typename == "" {
		return "", false
	}
	return typename, true
}
