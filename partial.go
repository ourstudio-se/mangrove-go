package mangrove

import (
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

type partialRecacheQuery struct {
	// linkSelections are the selection sets to use for link queries, by
	// coordinates.
	linkSelections map[string]ast.SelectionSet
	// prunedCoordinates are the coordinates whose selection sets were
	// narrowed down.
	prunedCoordinates []string
	query             *ast.QueryDocument
	// resolutions are the entities requested by each cache resolution.
	resolutions ResolutionExpectations
}

func isMarkerField(selection ast.Selection) (*ast.Field, bool) {
	field, ok := selection.(*ast.Field)
	if !ok || field.Alias == "" || field.Alias == field.Name || !isMarkerKey(field.Alias) {
		return nil, false
	}
	return field, true
}

func linkSelectionsOf(selectionSet ast.SelectionSet) ast.SelectionSet {
	var out ast.SelectionSet
	for _, selection := range selectionSet {
		if field, ok := isMarkerField(selection); ok {
			out = append(out, &ast.Field{
				Alias:    field.Alias,
				Name:     field.Name,
				Position: field.Position,
			})
			continue
		}
		if fragment, ok := selection.(*ast.InlineFragment); ok {
			inner := linkSelectionsOf(fragment.SelectionSet)
			if len(inner) == 0 {
				continue
			}
			copied := *fragment
			copied.SelectionSet = inner
			out = append(out, &copied)
		}
	}
	return out
}

func hasIDMarker(selections ast.SelectionSet) bool {
	for _, selection := range selections {
		switch s := selection.(type) {
		case *ast.Field:
			if s.Alias != "" && s.Alias != s.Name && isIDMarkerKey(s.Alias) {
				return true
			}
		case *ast.InlineFragment:
			if hasIDMarker(s.SelectionSet) {
				return true
			}
		}
	}
	return false
}

// convertFieldToLink replaces a field's selection set with one that only
// identifies the entity (its typename and id, inside the same type
// conditions as in the original). Returns nil if the selection set doesn't
// identify the entity.
func convertFieldToLink(field *ast.Field) *ast.Field {
	if len(field.SelectionSet) == 0 {
		return nil
	}

	selections := linkSelectionsOf(field.SelectionSet)

	if !hasIDMarker(selections) {
		return nil
	}

	link := *field
	link.SelectionSet = selections
	return &link
}

// selectionSetForType narrows a selection set down to what applies to
// resolver's type (typename), dropping inline fragments that can't apply to
// it. If the resolver knows its type conditions (resolvers configured from a
// schema do), only fragments on those are kept; otherwise fragments on other
// known entity types are dropped.
func selectionSetForType(selectionSet ast.SelectionSet, typename string, resolver CacheResolver, otherTypenames []string) ast.SelectionSet {
	var applies func(condition string) bool
	if resolver.TypeConditions != nil {
		applies = func(condition string) bool {
			if condition == typename {
				return true
			}
			for _, c := range resolver.TypeConditions {
				if c == condition {
					return true
				}
			}
			return false
		}
	} else {
		others := map[string]bool{}
		for _, other := range otherTypenames {
			others[other] = true
		}
		delete(others, typename)
		applies = func(condition string) bool { return !others[condition] }
	}

	var narrow func(ast.SelectionSet) ast.SelectionSet
	narrow = func(set ast.SelectionSet) ast.SelectionSet {
		out := make(ast.SelectionSet, 0, len(set))
		for _, selection := range set {
			fragment, ok := selection.(*ast.InlineFragment)
			if !ok {
				out = append(out, selection)
				continue
			}
			if fragment.TypeCondition != "" {
				if applies(fragment.TypeCondition) {
					out = append(out, selection)
				}
				continue
			}
			// Without a type condition the fragment applies to the same type,
			// so its own fragments need narrowing too.
			inner := narrow(fragment.SelectionSet)
			if len(inner) > 0 {
				copied := *fragment
				copied.SelectionSet = inner
				out = append(out, &copied)
			}
		}
		return out
	}

	return narrow(selectionSet)
}

func nodeCanLink(node *entityTreeNode) bool {
	return node != nil && node.linkable
}

func selectOperation(document *ast.QueryDocument, operationName string) (*ast.OperationDefinition, error) {
	inlined, err := inlineFragments(document)
	if err != nil {
		return nil, err
	}
	return getOperation(inlined, operationName), nil
}

// collectLinkSelections collects the selection set of every linkable field
// in the unpruned operation, with nested linkable fields already turned into
// links. These are used to build link queries, which need the full selection
// set.
func collectLinkSelections(operation *ast.OperationDefinition, tree *entityTree) map[string]ast.SelectionSet {
	linkSelections := map[string]ast.SelectionSet{}

	var visitSelectionSet func(ast.SelectionSet, string) ast.SelectionSet
	visitField := func(field *ast.Field, coordinates string) *ast.Field {
		if len(field.SelectionSet) == 0 {
			return field
		}
		visited := *field
		visited.SelectionSet = visitSelectionSet(field.SelectionSet, coordinates)

		if !nodeCanLink(tree.nodes[coordinates]) {
			return &visited
		}
		link := convertFieldToLink(&visited)
		if link == nil {
			return &visited
		}
		linkSelections[coordinates] = visited.SelectionSet
		return link
	}

	visitSelectionSet = func(selectionSet ast.SelectionSet, coordinates string) ast.SelectionSet {
		out := make(ast.SelectionSet, len(selectionSet))
		for i, selection := range selectionSet {
			switch s := selection.(type) {
			case *ast.Field:
				out[i] = visitField(s, coordinates+"."+responseKey(s))
			case *ast.InlineFragment:
				copied := *s
				copied.SelectionSet = visitSelectionSet(s.SelectionSet, coordinates)
				out[i] = &copied
			default:
				out[i] = selection
			}
		}
		return out
	}

	visitSelectionSet(operation.SelectionSet, coordinateRoot[string(operation.Operation)])

	return linkSelections
}

func getPartialRecacheQuery(originalDocument *ast.QueryDocument, tree *entityTree, operationName string) (*partialRecacheQuery, error) {
	operation, err := selectOperation(originalDocument, operationName)
	if err != nil {
		return nil, err
	}

	if operation == nil {
		if operationName != "" {
			return nil, fmt.Errorf("unknown operation %q", operationName)
		}
		return nil, fmt.Errorf("must provide operation name if query contains multiple operations")
	}

	if operation.Operation != ast.Query {
		return nil, fmt.Errorf("can't partially re-execute a %s", operation.Operation)
	}

	linkSelections := collectLinkSelections(operation, tree)
	aliases := newResolutionAliases()
	var cacheResolutions []ast.Selection
	var prunedCoordinates []string
	var rewriteErr error

	var rewriteSelectionSet func(ast.SelectionSet, string) ast.SelectionSet

	// rewriteField returns nil if the field is to be removed.
	rewriteField := func(field *ast.Field, coordinates string) *ast.Field {
		if len(field.SelectionSet) == 0 {
			return field
		}

		rewritten := *field
		rewritten.SelectionSet = rewriteSelectionSet(field.SelectionSet, coordinates)

		if len(rewritten.SelectionSet) == 0 {
			// Removing the field narrows the parent's selection set as well.
			prunedCoordinates = append(prunedCoordinates, coordinates[:strings.LastIndex(coordinates, ".")])
			return nil
		}

		node := tree.nodes[coordinates]

		if !nodeCanLink(node) {
			return &rewritten
		}

		link := convertFieldToLink(&rewritten)

		if link == nil {
			return &rewritten
		}

		otherTypenames := append(append([]string{}, node.typenameOrder...), tree.resolverTypenames...)

		for _, typename := range node.requiredEntities.typenames {
			resolver, ok := node.resolvers[typename]

			if !ok {
				continue
			}

			selectionSet := selectionSetForType(rewritten.SelectionSet, typename, resolver, otherTypenames)

			resolutions, err := mapCacheResolutions(
				resolver,
				typename,
				node.requiredEntities.ids[typename],
				coordinates,
				selectionSet,
				aliases,
			)
			if err != nil && rewriteErr == nil {
				rewriteErr = err
			}
			cacheResolutions = append(cacheResolutions, resolutions...)
		}

		return link
	}

	rewriteSelectionSet = func(selectionSet ast.SelectionSet, coordinates string) ast.SelectionSet {
		out := make(ast.SelectionSet, 0, len(selectionSet))
		for _, selection := range selectionSet {
			switch s := selection.(type) {
			case *ast.Field:
				if rewritten := rewriteField(s, coordinates+"."+responseKey(s)); rewritten != nil {
					out = append(out, rewritten)
				}
			case *ast.InlineFragment:
				inner := rewriteSelectionSet(s.SelectionSet, coordinates)
				// An empty selection set is not allowed in GraphQL: the
				// fragment is removed entirely.
				if len(inner) == 0 {
					continue
				}
				copied := *s
				copied.SelectionSet = inner
				out = append(out, &copied)
			default:
				out = append(out, selection)
			}
		}

		keep := getSelectionsToKeep(tree.nodes[coordinates])
		if keep == nil {
			return out
		}
		prunedCoordinates = append(prunedCoordinates, coordinates)
		return pruneSelectionSet(out, keep)
	}

	rewritten := rewriteSelectionSet(operation.SelectionSet, coordinateRoot[string(operation.Operation)])

	if rewriteErr != nil {
		return nil, rewriteErr
	}

	selections := append(append(ast.SelectionSet{}, rewritten...), cacheResolutions...)

	// Nothing but the root typename (or nothing at all) left to fetch.
	fetchesSomething := false
	for _, selection := range selections {
		if field, ok := selection.(*ast.Field); ok && field.Alias == AliasEntityCacheTypename {
			continue
		}
		fetchesSomething = true
		break
	}

	if !fetchesSomething {
		return nil, nil
	}

	query := *operation
	query.SelectionSet = selections

	return &partialRecacheQuery{
		linkSelections:    linkSelections,
		prunedCoordinates: prunedCoordinates,
		query:             &ast.QueryDocument{Operations: ast.OperationList{pruneUnusedVariables(&query)}},
		resolutions:       aliases.expectations,
	}, nil
}
