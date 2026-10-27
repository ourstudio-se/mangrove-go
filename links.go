package mangrove

import (
	"fmt"

	"github.com/vektah/gqlparser/v2/ast"
)

// makeLinkCollector creates a function that finds the link stubs in an
// execution result that point to entities we have no data for at that
// position. Each entity is only ever returned once per collector.
func makeLinkCollector(snapshots *entitySnapshots, linkCoordinates map[string]bool) func(data *Object) ([]linkGroup, error) {
	requested := map[string]bool{}

	return func(data *Object) ([]linkGroup, error) {
		var groups []*linkGroup
		groupIndex := map[string]*linkGroup{}

		var walk func(value any, coordinates string)
		walk = func(value any, coordinates string) {
			switch v := value.(type) {
			case []any:
				for _, item := range v {
					walk(item, coordinates)
				}
			case *Object:
				if v == nil {
					return
				}
				if linkCoordinates[coordinates] && isLinkStub(v) {
					identity, _ := getEntityIdentity(v)
					key := coordinates + "\x00" + identity

					if snapshots.has(coordinates, identity) || requested[key] {
						return
					}

					requested[key] = true

					typename, _ := getMarkedTypename(v)
					id, _ := getMarkedID(v)
					groupKey := coordinates + "\x00" + typename

					group := groupIndex[groupKey]
					if group == nil {
						group = &linkGroup{coordinates: coordinates, typename: typename}
						groupIndex[groupKey] = group
						groups = append(groups, group)
					}
					group.ids = append(group.ids, id)
					return
				}

				for _, key := range v.keys {
					walk(v.values[key], coordinates+"."+key)
				}
			}
		}

		for _, key := range data.Keys() {
			coordinates := rootQuery + "." + key
			if isCacheAliasName(key) {
				parsed, err := parseCacheResolverAlias(key)
				if err != nil {
					return nil, err
				}
				coordinates = parsed
			}
			walk(data.values[key], coordinates)
		}

		out := make([]linkGroup, len(groups))
		for i, group := range groups {
			out[i] = *group
		}
		return out, nil
	}
}

type linkQuery struct {
	document      *ast.QueryDocument
	operationName string
	resolutions   ResolutionExpectations
}

// buildLinkQuery builds a query fetching every linked entity through its
// cache resolver, using the selection set found at the link's coordinates in
// the original document.
func buildLinkQuery(
	links []linkGroup,
	linkSelections map[string]ast.SelectionSet,
	resolvers CacheResolverMap,
	originalOperationName string,
	variableDefinitions ast.VariableDefinitionList,
) (*linkQuery, error) {
	aliases := newResolutionAliases()
	var cacheResolutions ast.SelectionSet
	resolverTypenames := make([]string, 0, len(resolvers))
	for typename := range resolvers {
		resolverTypenames = append(resolverTypenames, typename)
	}

	for _, link := range links {
		resolver, ok := resolvers[link.typename]

		if !ok {
			return nil, &CacheMergeError{Message: fmt.Sprintf("No cache resolver for linked entity type %s at %s", link.typename, link.coordinates)}
		}

		selectionSet, ok := linkSelections[link.coordinates]

		if !ok {
			return nil, &CacheMergeError{Message: fmt.Sprintf("Selection set not found for link with coordinates %s", link.coordinates)}
		}

		resolutions, err := mapCacheResolutions(
			resolver,
			link.typename,
			link.ids,
			link.coordinates,
			selectionSetForType(selectionSet, link.typename, resolver, resolverTypenames),
			aliases,
		)
		if err != nil {
			return nil, err
		}
		cacheResolutions = append(cacheResolutions, resolutions...)
	}

	if len(cacheResolutions) == 0 {
		return nil, nil
	}

	operationName := "_linkQuery"
	if originalOperationName != "" {
		operationName = originalOperationName + "__linkQuery"
	}

	operation := pruneUnusedVariables(&ast.OperationDefinition{
		Operation:           ast.Query,
		Name:                operationName,
		SelectionSet:        cacheResolutions,
		VariableDefinitions: variableDefinitions,
	})

	return &linkQuery{
		document:      &ast.QueryDocument{Operations: ast.OperationList{operation}},
		operationName: operationName,
		resolutions:   aliases.expectations,
	}, nil
}
