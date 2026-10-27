package mangrove

import (
	"fmt"
	"regexp"

	"github.com/vektah/gqlparser/v2/ast"
)

var (
	intPattern   = regexp.MustCompile(`^-?(?:0|[1-9]\d*)$`)
	floatPattern = regexp.MustCompile(`^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$`)
)

// idToValue builds the literal for an entity id. Values are never
// interpolated into GraphQL source text.
func idToValue(resolver CacheResolver, id string) (*ast.Value, error) {
	switch resolver.Type {
	case KeyTypeInt:
		if !intPattern.MatchString(id) {
			return nil, &CacheResolutionError{Message: fmt.Sprintf("Id %q is not a valid Int for %s(%s:)", id, resolver.RootField, resolver.IDArg)}
		}
		return &ast.Value{Kind: ast.IntValue, Raw: id}, nil
	case KeyTypeFloat:
		if !floatPattern.MatchString(id) {
			return nil, &CacheResolutionError{Message: fmt.Sprintf("Id %q is not a valid Float for %s(%s:)", id, resolver.RootField, resolver.IDArg)}
		}
		return &ast.Value{Kind: ast.FloatValue, Raw: id}, nil
	default:
		return &ast.Value{Kind: ast.StringValue, Raw: id}, nil
	}
}

// resolutionAliases hands out aliases that are unique within one generated
// document.
type resolutionAliases struct {
	// expectations holds what each alias requested, to verify the response
	// against.
	expectations ResolutionExpectations
	next         int
}

func newResolutionAliases() *resolutionAliases {
	return &resolutionAliases{expectations: ResolutionExpectations{}}
}

func (a *resolutionAliases) alias(coordinates, typename string, ids []string) string {
	alias := getCacheResolverAlias(coordinates, a.next)
	a.next++
	a.expectations[alias] = Expectation{IDs: ids, Typename: typename}
	return alias
}

func mapCacheResolutions(
	resolver CacheResolver,
	typename string,
	ids []string,
	coordinates string,
	selectionSet ast.SelectionSet,
	aliases *resolutionAliases,
) ([]ast.Selection, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// Root fields returning an interface or union only accept the selections
	// of the resolved type inside a fragment on it.
	resolvedSelectionSet := selectionSet
	if resolver.Abstract {
		resolvedSelectionSet = ast.SelectionSet{&ast.InlineFragment{
			TypeCondition: typename,
			SelectionSet:  selectionSet,
		}}
	}

	field := func(value *ast.Value, requested []string) *ast.Field {
		return &ast.Field{
			Alias: aliases.alias(coordinates, typename, requested),
			Name:  resolver.RootField,
			Arguments: ast.ArgumentList{{
				Name:  resolver.IDArg,
				Value: value,
			}},
			SelectionSet: resolvedSelectionSet,
		}
	}

	if resolver.Batch {
		list := &ast.Value{Kind: ast.ListValue}
		for _, id := range ids {
			value, err := idToValue(resolver, id)
			if err != nil {
				return nil, err
			}
			list.Children = append(list.Children, &ast.ChildValue{Value: value})
		}
		return []ast.Selection{field(list, ids)}, nil
	}

	selections := make([]ast.Selection, 0, len(ids))
	for _, id := range ids {
		value, err := idToValue(resolver, id)
		if err != nil {
			return nil, err
		}
		selections = append(selections, field(value, []string{id}))
	}
	return selections, nil
}
