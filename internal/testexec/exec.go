// Package testexec is a small GraphQL executor for tests, executing
// documents against a schema with resolvers given as plain functions.
package testexec

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	// Registers the standard validation rules.
	_ "github.com/vektah/gqlparser/v2/validator/rules"
)

// ResolverFunc resolves a field.
type ResolverFunc func(ctx context.Context, parent any, args map[string]any) (any, error)

// Resolvers holds resolvers by type and field. Fields without a resolver
// read the field from a map[string]any parent.
type Resolvers map[string]map[string]ResolverFunc

// Schema is an executable schema.
type Schema struct {
	Schema    *ast.Schema
	Resolvers Resolvers
	// ResolveType returns the concrete type of a value of an abstract type.
	// Defaults to the "__typename" key of a map[string]any value.
	ResolveType func(value any) string
}

// MustSchema loads a schema from SDL.
func MustSchema(sdl string, resolvers Resolvers) *Schema {
	schema, err := gqlparser.LoadSchema(&ast.Source{Name: "schema.graphql", Input: sdl})
	if err != nil {
		panic(err)
	}
	return &Schema{Schema: schema, Resolvers: resolvers}
}

// Parse parses and validates a document.
func (s *Schema) Parse(query string) (*ast.QueryDocument, error) {
	document, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		return nil, err
	}
	if errs := validator.ValidateWithRules(s.Schema, document, nil); len(errs) > 0 {
		return nil, errs
	}
	return document, nil
}

// MustParse parses and validates a document, panicking on errors.
func (s *Schema) MustParse(query string) *ast.QueryDocument {
	document, err := s.Parse(query)
	if err != nil {
		panic(fmt.Sprintf("%v\n%s", err, query))
	}
	return document
}

// Executor returns a Mangrove executor. Every document is printed, parsed
// and validated first, the way a remote server would.
func (s *Schema) Executor() mangrove.Executor {
	return func(ctx context.Context, request *mangrove.Request) (*mangrove.Response, error) {
		printed := mangrove.PrintDocument(request.Document)
		document, err := s.Parse(printed)
		if err != nil {
			return nil, fmt.Errorf("invalid document: %w\n%s", err, printed)
		}
		return s.Execute(ctx, document, request.OperationName, request.Variables)
	}
}

// Execute executes a validated document.
func (s *Schema) Execute(ctx context.Context, document *ast.QueryDocument, operationName string, variables map[string]any) (*mangrove.Response, error) {
	var operation *ast.OperationDefinition
	if operationName == "" {
		if len(document.Operations) != 1 {
			return &mangrove.Response{Errors: gqlerror.List{gqlerror.Errorf("must provide operation name if query contains multiple operations")}}, nil
		}
		operation = document.Operations[0]
	} else {
		operation = document.Operations.ForName(operationName)
		if operation == nil {
			return &mangrove.Response{Errors: gqlerror.List{gqlerror.Errorf("unknown operation named %q", operationName)}}, nil
		}
	}

	coerced, err := validator.VariableValues(s.Schema, operation, variables)
	if err != nil {
		return &mangrove.Response{Errors: gqlerror.List{gqlerror.WrapIfUnwrapped(err)}}, nil
	}

	e := &execution{schema: s, document: document, variables: coerced}

	var root *ast.Definition
	switch operation.Operation {
	case ast.Mutation:
		root = s.Schema.Mutation
	case ast.Subscription:
		root = s.Schema.Subscription
	default:
		root = s.Schema.Query
	}

	data, ok := e.executeSelectionSet(ctx, operation.SelectionSet, root, nil, ast.Path{})
	response := &mangrove.Response{Errors: e.errors}
	if ok {
		encoded, err := mangrove.EncodeJSON(data)
		if err != nil {
			return nil, err
		}
		response.Data = encoded
	} else {
		response.Data = json.RawMessage("null")
	}
	return response, nil
}

type execution struct {
	schema    *Schema
	document  *ast.QueryDocument
	variables map[string]any
	errors    gqlerror.List
}

type collectedField struct {
	key    string
	fields []*ast.Field
}

func (e *execution) skip(directives ast.DirectiveList) bool {
	if d := directives.ForName("skip"); d != nil {
		if value, _ := d.ArgumentMap(e.variables)["if"].(bool); value {
			return true
		}
	}
	if d := directives.ForName("include"); d != nil {
		if value, _ := d.ArgumentMap(e.variables)["if"].(bool); !value {
			return true
		}
	}
	return false
}

func (e *execution) typeApplies(object *ast.Definition, condition string) bool {
	if condition == "" || condition == object.Name {
		return true
	}
	definition := e.schema.Schema.Types[condition]
	if definition == nil {
		return false
	}
	for _, possible := range e.schema.Schema.GetPossibleTypes(definition) {
		if possible.Name == object.Name {
			return true
		}
	}
	return false
}

func (e *execution) collectFields(object *ast.Definition, selectionSet ast.SelectionSet, out *[]*collectedField, index map[string]*collectedField, visited map[string]bool) {
	for _, selection := range selectionSet {
		switch s := selection.(type) {
		case *ast.Field:
			if e.skip(s.Directives) {
				continue
			}
			key := s.Alias
			if key == "" {
				key = s.Name
			}
			if existing := index[key]; existing != nil {
				existing.fields = append(existing.fields, s)
				continue
			}
			field := &collectedField{key: key, fields: []*ast.Field{s}}
			index[key] = field
			*out = append(*out, field)
		case *ast.InlineFragment:
			if e.skip(s.Directives) || !e.typeApplies(object, s.TypeCondition) {
				continue
			}
			e.collectFields(object, s.SelectionSet, out, index, visited)
		case *ast.FragmentSpread:
			if e.skip(s.Directives) || visited[s.Name] {
				continue
			}
			visited[s.Name] = true
			fragment := e.document.Fragments.ForName(s.Name)
			if fragment == nil || !e.typeApplies(object, fragment.TypeCondition) {
				continue
			}
			e.collectFields(object, fragment.SelectionSet, out, index, visited)
		}
	}
}

// executeSelectionSet returns false if the object resolved to null because
// of an error in a non-null field.
func (e *execution) executeSelectionSet(ctx context.Context, selectionSet ast.SelectionSet, object *ast.Definition, parent any, path ast.Path) (*mangrove.Object, bool) {
	var fields []*collectedField
	e.collectFields(object, selectionSet, &fields, map[string]*collectedField{}, map[string]bool{})

	out := mangrove.NewObject(len(fields))
	for _, field := range fields {
		first := field.fields[0]
		fieldPath := append(append(ast.Path{}, path...), ast.PathName(field.key))

		if first.Name == "__typename" {
			out.Set(field.key, object.Name)
			continue
		}

		definition := object.Fields.ForName(first.Name)
		if definition == nil {
			e.errors = append(e.errors, &gqlerror.Error{Message: fmt.Sprintf("unknown field %s.%s", object.Name, first.Name), Path: fieldPath})
			return nil, false
		}

		value, err := e.resolve(ctx, object, definition, first, parent)
		if err != nil {
			e.errors = append(e.errors, &gqlerror.Error{Message: err.Error(), Path: fieldPath})
			if definition.Type.NonNull {
				return nil, false
			}
			out.Set(field.key, nil)
			continue
		}

		var subSelections ast.SelectionSet
		for _, f := range field.fields {
			subSelections = append(subSelections, f.SelectionSet...)
		}

		completed, ok := e.complete(ctx, definition.Type, subSelections, value, fieldPath)
		if !ok {
			if definition.Type.NonNull {
				return nil, false
			}
			out.Set(field.key, nil)
			continue
		}
		out.Set(field.key, completed)
	}
	return out, true
}

func (e *execution) resolve(ctx context.Context, object *ast.Definition, definition *ast.FieldDefinition, field *ast.Field, parent any) (any, error) {
	args := field.ArgumentMap(e.variables)
	for _, argument := range definition.Arguments {
		if _, ok := args[argument.Name]; !ok && argument.DefaultValue != nil {
			value, err := argument.DefaultValue.Value(e.variables)
			if err == nil {
				args[argument.Name] = value
			}
		}
	}
	if resolver := e.schema.Resolvers[object.Name][definition.Name]; resolver != nil {
		return resolver(ctx, parent, args)
	}
	if m, ok := parent.(map[string]any); ok {
		return m[definition.Name], nil
	}
	return nil, nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	switch v := value.(type) {
	case map[string]any:
		return v == nil
	case []any:
		return v == nil
	}
	return false
}

// complete returns false if the value is null where it can't be, an error
// having been recorded.
func (e *execution) complete(ctx context.Context, t *ast.Type, selectionSet ast.SelectionSet, value any, path ast.Path) (any, bool) {
	if isNil(value) {
		if t.NonNull {
			e.errors = append(e.errors, &gqlerror.Error{Message: "Cannot return null for non-nullable field", Path: path})
			return nil, false
		}
		return nil, true
	}

	if t.Elem != nil {
		items, ok := toList(value)
		if !ok {
			e.errors = append(e.errors, &gqlerror.Error{Message: "Expected a list", Path: path})
			return nil, !t.NonNull
		}
		out := make([]any, len(items))
		for i, item := range items {
			itemPath := append(append(ast.Path{}, path...), ast.PathIndex(i))
			completed, ok := e.complete(ctx, t.Elem, selectionSet, item, itemPath)
			if !ok {
				return nil, !t.NonNull
			}
			out[i] = completed
		}
		return out, true
	}

	definition := e.schema.Schema.Types[t.NamedType]
	switch definition.Kind {
	case ast.Scalar, ast.Enum:
		serialized, err := serialize(definition.Name, value)
		if err != nil {
			e.errors = append(e.errors, &gqlerror.Error{Message: err.Error(), Path: path})
			return nil, !t.NonNull
		}
		return serialized, true
	case ast.Interface, ast.Union:
		typename := e.resolveType(value)
		concrete := e.schema.Schema.Types[typename]
		if concrete == nil || !e.typeApplies(concrete, definition.Name) {
			e.errors = append(e.errors, &gqlerror.Error{Message: fmt.Sprintf("Could not resolve the type of %s", definition.Name), Path: path})
			return nil, !t.NonNull
		}
		definition = concrete
	}

	object, ok := e.executeSelectionSet(ctx, selectionSet, definition, value, path)
	if !ok {
		return nil, !t.NonNull
	}
	return object, true
}

func (e *execution) resolveType(value any) string {
	if e.schema.ResolveType != nil {
		return e.schema.ResolveType(value)
	}
	if m, ok := value.(map[string]any); ok {
		typename, _ := m["__typename"].(string)
		return typename
	}
	return ""
}

func toList(value any) ([]any, bool) {
	switch v := value.(type) {
	case []any:
		return v, true
	case []map[string]any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = item
		}
		return out, true
	case []string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = item
		}
		return out, true
	}
	return nil, false
}

func serialize(scalar string, value any) (any, error) {
	switch scalar {
	case "Int":
		switch v := value.(type) {
		case int:
			return json.Number(strconv.Itoa(v)), nil
		case int64:
			return json.Number(strconv.FormatInt(v, 10)), nil
		case float64:
			if v != math.Trunc(v) {
				return nil, fmt.Errorf("Int cannot represent non-integer value: %v", v)
			}
			return json.Number(strconv.FormatInt(int64(v), 10)), nil
		case json.Number:
			return v, nil
		}
		return nil, fmt.Errorf("Int cannot represent %T", value)
	case "Float":
		switch v := value.(type) {
		case int:
			return json.Number(strconv.Itoa(v)), nil
		case int64:
			return json.Number(strconv.FormatInt(v, 10)), nil
		case float64:
			return json.Number(strconv.FormatFloat(v, 'g', -1, 64)), nil
		case json.Number:
			return v, nil
		}
		return nil, fmt.Errorf("Float cannot represent %T", value)
	case "ID":
		switch v := value.(type) {
		case string:
			return v, nil
		case int:
			return strconv.Itoa(v), nil
		case int64:
			return strconv.FormatInt(v, 10), nil
		case json.Number:
			return string(v), nil
		}
		return nil, fmt.Errorf("ID cannot represent %T", value)
	case "Boolean":
		if v, ok := value.(bool); ok {
			return v, nil
		}
		return nil, fmt.Errorf("Boolean cannot represent %T", value)
	default:
		switch v := value.(type) {
		case string, bool, json.Number:
			return v, nil
		case int:
			return json.Number(strconv.Itoa(v)), nil
		case int64:
			return json.Number(strconv.FormatInt(v, 10)), nil
		case float64:
			return json.Number(strconv.FormatFloat(v, 'g', -1, 64)), nil
		}
		return nil, fmt.Errorf("%s cannot represent %T", scalar, value)
	}
}
