package mangrove

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

func responseKey(field *ast.Field) string {
	if field.Alias != "" {
		return field.Alias
	}
	return field.Name
}

func pruneSelectionSet(selectionSet ast.SelectionSet, keep []string) ast.SelectionSet {
	keeps := func(key string) bool {
		for _, k := range keep {
			if k == key {
				return true
			}
		}
		return false
	}
	keepsMarkers := keeps(AliasEntityCacheID)

	out := make(ast.SelectionSet, 0, len(selectionSet))
	for _, selection := range selectionSet {
		if field, ok := selection.(*ast.Field); ok {
			key := responseKey(field)
			if !keeps(key) && !(isMarkerKey(key) && keepsMarkers) {
				continue
			}
		}
		out = append(out, selection)
	}
	return out
}

/*
 * Human readable path notation, used for debugging and in tests:
 *   Query.items~Item@0#"1".owner
 * `~Type` is the entity type at that position, `@n` the list index and `#…`
 * the JSON encoded entity id. Ids may contain any character.
 */

func dataPathPartToStr(part PathPart) string {
	str := part.Field
	if part.Typename != "" {
		str += "~" + part.Typename
	}
	if part.Index != nil {
		str += "@" + strconv.Itoa(*part.Index)
	}
	if part.ID != nil {
		encoded, _ := json.Marshal(*part.ID)
		str += "#" + string(encoded)
	}
	return str
}

func splitPathString(str string) []string {
	var parts []string
	var current strings.Builder
	inString := false

	for i := 0; i < len(str); i++ {
		char := str[i]

		if inString {
			current.WriteByte(char)
			if char == '\\' {
				if i+1 < len(str) {
					i++
					current.WriteByte(str[i])
				}
			} else if char == '"' {
				inString = false
			}
			continue
		}

		switch char {
		case '"':
			inString = true
			current.WriteByte(char)
		case '.':
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteByte(char)
		}
	}

	return append(parts, current.String())
}

var pathPartPattern = regexp.MustCompile(`^([^~@#]+)(?:~([^@#]+))?(?:@(\d+))?(?:#(.+))?$`)

func strToDataPathPart(str string) (PathPart, error) {
	match := pathPartPattern.FindStringSubmatchIndex(str)
	if match == nil {
		return PathPart{}, fmt.Errorf("invalid data path part: %s", str)
	}
	group := func(n int) (string, bool) {
		if match[2*n] < 0 {
			return "", false
		}
		return str[match[2*n]:match[2*n+1]], true
	}

	field, _ := group(1)
	part := PathPart{Field: field}

	if typename, ok := group(2); ok {
		part.Typename = typename
	}
	if index, ok := group(3); ok {
		n, err := strconv.Atoi(index)
		if err != nil {
			return PathPart{}, err
		}
		part.Index = &n
	}
	if encoded, ok := group(4); ok {
		value, err := DecodeJSON([]byte(encoded))
		if err != nil {
			return PathPart{}, err
		}
		var id string
		switch v := value.(type) {
		case string:
			id = v
		case json.Number:
			id = string(v)
		default:
			return PathPart{}, fmt.Errorf("invalid id in data path part: %s", str)
		}
		part.ID = &id
	}

	return part, nil
}

// DataPathToStr prints a data path, e.g. `Query.items~Item@0#"1".owner`.
func DataPathToStr(path []PathPart) string {
	parts := make([]string, len(path))
	for i, part := range path {
		parts[i] = dataPathPartToStr(part)
	}
	return strings.Join(parts, ".")
}

// StrToDataPath parses a data path printed by DataPathToStr.
func StrToDataPath(str string) ([]PathPart, error) {
	if str == "" {
		return nil, fmt.Errorf("empty string cannot be a data path")
	}
	strs := splitPathString(str)
	path := make([]PathPart, len(strs))
	for i, s := range strs {
		part, err := strToDataPathPart(s)
		if err != nil {
			return nil, err
		}
		path[i] = part
	}
	return path, nil
}

func pathToCoordinates(path []PathPart) string {
	fields := make([]string, len(path))
	for i, part := range path {
		fields[i] = part.Field
	}
	return strings.Join(fields, ".")
}

// cleanupData returns a copy of data without Mangrove's internal marker
// fields.
func cleanupData(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cleanupData(item)
		}
		return out
	case *Object:
		if v == nil {
			return v
		}
		out := NewObject(len(v.fields))
		for _, f := range v.fields {
			if isMarkerKey(f.key) {
				continue
			}
			out.fields = append(out.fields, field{key: f.key, value: cleanupData(f.value)})
		}
		return out
	default:
		return value
	}
}

func getOperation(document *ast.QueryDocument, operationName string) *ast.OperationDefinition {
	if operationName != "" {
		for _, operation := range document.Operations {
			if operation.Name == operationName {
				return operation
			}
		}
		return nil
	}
	if len(document.Operations) == 1 {
		return document.Operations[0]
	}
	return nil
}

func isIntrospectionQuery(operation *ast.OperationDefinition) bool {
	if operation.Operation != ast.Query {
		return false
	}
	for _, selection := range operation.SelectionSet {
		field, ok := selection.(*ast.Field)
		if !ok || (field.Name != "__schema" && field.Name != "__type" && field.Name != "__typename") {
			return false
		}
	}
	return true
}

func isIntrospectionDocument(document *ast.QueryDocument) bool {
	for _, operation := range document.Operations {
		if !isIntrospectionQuery(operation) {
			return false
		}
	}
	return true
}

// inlineFragments replaces every fragment spread with an equivalent inline
// fragment (keeping the spread's directives) and drops the fragment
// definitions.
func inlineFragments(document *ast.QueryDocument) (*ast.QueryDocument, error) {
	if len(document.Fragments) == 0 {
		return document, nil
	}

	var flattenSelectionSet func(ast.SelectionSet, []string) (ast.SelectionSet, error)

	flattenSelection := func(selection ast.Selection, visiting []string) (ast.Selection, error) {
		switch s := selection.(type) {
		case *ast.Field:
			if len(s.SelectionSet) == 0 {
				return s, nil
			}
			selectionSet, err := flattenSelectionSet(s.SelectionSet, visiting)
			if err != nil {
				return nil, err
			}
			field := *s
			field.SelectionSet = selectionSet
			return &field, nil
		case *ast.InlineFragment:
			selectionSet, err := flattenSelectionSet(s.SelectionSet, visiting)
			if err != nil {
				return nil, err
			}
			fragment := *s
			fragment.SelectionSet = selectionSet
			return &fragment, nil
		case *ast.FragmentSpread:
			definition := document.Fragments.ForName(s.Name)
			if definition == nil {
				return nil, fmt.Errorf("fragment definition does not exist: %s", s.Name)
			}
			for _, name := range visiting {
				if name == s.Name {
					return nil, fmt.Errorf("cyclic fragment spread: %s", s.Name)
				}
			}
			nested := append(append(make([]string, 0, len(visiting)+1), visiting...), s.Name)
			selectionSet, err := flattenSelectionSet(definition.SelectionSet, nested)
			if err != nil {
				return nil, err
			}
			return &ast.InlineFragment{
				TypeCondition:    definition.TypeCondition,
				Directives:       s.Directives,
				SelectionSet:     selectionSet,
				ObjectDefinition: definition.Definition,
				Position:         s.Position,
			}, nil
		default:
			return selection, nil
		}
	}

	flattenSelectionSet = func(selectionSet ast.SelectionSet, visiting []string) (ast.SelectionSet, error) {
		out := make(ast.SelectionSet, len(selectionSet))
		for i, selection := range selectionSet {
			flattened, err := flattenSelection(selection, visiting)
			if err != nil {
				return nil, err
			}
			out[i] = flattened
		}
		return out, nil
	}

	operations := make(ast.OperationList, len(document.Operations))
	for i, operation := range document.Operations {
		selectionSet, err := flattenSelectionSet(operation.SelectionSet, nil)
		if err != nil {
			return nil, err
		}
		flattened := *operation
		flattened.SelectionSet = selectionSet
		operations[i] = &flattened
	}

	return &ast.QueryDocument{Operations: operations, Position: document.Position}, nil
}

func collectVariables(value *ast.Value, used map[string]bool) {
	if value == nil {
		return
	}
	if value.Kind == ast.Variable {
		used[value.Raw] = true
		return
	}
	for _, child := range value.Children {
		collectVariables(child.Value, used)
	}
}

func collectDirectiveVariables(directives ast.DirectiveList, used map[string]bool) {
	for _, directive := range directives {
		for _, argument := range directive.Arguments {
			collectVariables(argument.Value, used)
		}
	}
}

func collectSelectionSetVariables(selectionSet ast.SelectionSet, used map[string]bool) {
	for _, selection := range selectionSet {
		switch s := selection.(type) {
		case *ast.Field:
			for _, argument := range s.Arguments {
				collectVariables(argument.Value, used)
			}
			collectDirectiveVariables(s.Directives, used)
			collectSelectionSetVariables(s.SelectionSet, used)
		case *ast.InlineFragment:
			collectDirectiveVariables(s.Directives, used)
			collectSelectionSetVariables(s.SelectionSet, used)
		case *ast.FragmentSpread:
			collectDirectiveVariables(s.Directives, used)
		}
	}
}

// pruneUnusedVariables removes variable definitions that are no longer
// referenced by an operation.
func pruneUnusedVariables(operation *ast.OperationDefinition) *ast.OperationDefinition {
	if len(operation.VariableDefinitions) == 0 {
		return operation
	}

	used := map[string]bool{}
	collectSelectionSetVariables(operation.SelectionSet, used)
	// Directives on the operation itself may reference variables too.
	collectDirectiveVariables(operation.Directives, used)

	pruned := *operation
	pruned.VariableDefinitions = nil
	for _, definition := range operation.VariableDefinitions {
		if used[definition.Variable] {
			pruned.VariableDefinitions = append(pruned.VariableDefinitions, definition)
		}
	}
	return &pruned
}

// collectEntityRecords walks result data and collects every entity together
// with its location.
//
// Paths start at `Query`. Descendants of an entity inherit the entity's
// annotated path part (typename/id), so that every entity knows which
// entities it was reached through.
func collectEntityRecords(data any, collect CollectEntityFunc) []EntityWithLocation {
	if collect == nil {
		collect = DefaultCollectEntity
	}

	var records []EntityWithLocation

	var walk func(value any, currentPath []PathPart)
	walk = func(value any, currentPath []PathPart) {
		switch v := value.(type) {
		case []any:
			if len(currentPath) == 0 {
				return
			}
			last := currentPath[len(currentPath)-1]
			prefix := currentPath[:len(currentPath)-1]
			for index, item := range v {
				i := index
				walk(item, appendPath(prefix, PathPart{Field: last.Field, Index: &i}))
			}
		case *Object:
			if v == nil {
				return
			}
			basePath := currentPath

			if record, ok := collect(v, currentPath); ok {
				records = append(records, record)
				basePath = record.Path
			}

			for _, f := range v.fields {
				if isMarkerKey(f.key) {
					continue
				}
				walk(f.value, appendPath(basePath, PathPart{Field: f.key}))
			}
		}
	}

	walk(data, []PathPart{{Field: rootQuery}})

	return records
}

// appendPath returns a new path; paths are shared between records and must
// never be appended to in place.
func appendPath(path []PathPart, part PathPart) []PathPart {
	out := make([]PathPart, len(path)+1)
	copy(out, path)
	out[len(path)] = part
	return out
}

// collectOpaqueCoordinates returns the coordinates at which data holds
// objects that aren't identifiable entities (the root object aside). Such
// positions can't be reached through links.
func collectOpaqueCoordinates(data any) []string {
	var found []string
	seen := map[string]bool{}

	var walk func(value any, coordinates string, isRoot bool)
	walk = func(value any, coordinates string, isRoot bool) {
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				walk(item, coordinates, false)
			}
		case *Object:
			if v == nil {
				return
			}
			if !isRoot {
				_, hasTypename := getMarkedTypename(v)
				_, hasID := getMarkedID(v)
				if (!hasTypename || !hasID) && !seen[coordinates] {
					seen[coordinates] = true
					found = append(found, coordinates)
				}
			}
			for _, f := range v.fields {
				if !isMarkerKey(f.key) {
					walk(f.value, coordinates+"."+f.key, false)
				}
			}
		}
	}

	walk(data, rootQuery, true)

	if found == nil {
		found = []string{}
	}
	return found
}

// knownEntities returns the ids of every entity, by type, in order of
// appearance.
func knownEntities(collected []EntityWithLocation) (map[string][]string, []string) {
	known := map[string][]string{}
	seen := map[string]map[string]bool{}
	var typenames []string

	for _, record := range collected {
		if record.Entity.ID == nil {
			continue
		}
		typename := record.Entity.Typename
		ids := seen[typename]
		if ids == nil {
			ids = map[string]bool{}
			seen[typename] = ids
			typenames = append(typenames, typename)
		}
		if !ids[*record.Entity.ID] {
			ids[*record.Entity.ID] = true
			known[typename] = append(known[typename], *record.Entity.ID)
		}
	}

	return known, typenames
}
