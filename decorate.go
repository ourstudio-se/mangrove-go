package mangrove

import (
	"github.com/vektah/gqlparser/v2/ast"
)

func typenameSelection() *ast.Field {
	return &ast.Field{Alias: AliasEntityCacheTypename, Name: "__typename"}
}

func idSelection(idField, alias string) *ast.Field {
	return &ast.Field{Alias: alias, Name: idField}
}

/*
 * Only documents consisting of queries are decorated: anything else is
 * executed without the cache, and fragments shared with mutations or
 * subscriptions would otherwise leak marker fields into their results.
 */
func isQueryOnlyDocument(document *ast.QueryDocument) bool {
	for _, operation := range document.Operations {
		if operation.Operation != ast.Query {
			return false
		}
	}
	return true
}

// isDecorated reports whether a document was decorated already. Every
// decorated operation selects the typename marker at its root, which
// servers that cache parsed documents may feed through the decorator again.
func isDecorated(document *ast.QueryDocument) bool {
	if len(document.Operations) == 0 {
		return false
	}
	for _, operation := range document.Operations {
		found := false
		for _, selection := range operation.SelectionSet {
			if field, ok := selection.(*ast.Field); ok && field.Alias == AliasEntityCacheTypename && field.Name == "__typename" {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type schemaDecorator struct {
	schema            *ast.Schema
	idFieldByTypename map[string]string
	memo              weakMemo[ast.QueryDocument, *ast.QueryDocument]
}

func (d *schemaDecorator) decorate(document *ast.QueryDocument) *ast.QueryDocument {
	return d.memo.get(document, d.decorateDocument)
}

// schemaIDSelection is the id marker for idField of a type, typed after the
// field's type.
func (d *schemaDecorator) schemaIDSelection(typename, idField string) *ast.Field {
	fieldType := ""
	if definition := d.schema.Types[typename]; definition != nil &&
		(definition.Kind == ast.Object || definition.Kind == ast.Interface) {
		if field := definition.Fields.ForName(idField); field != nil {
			fieldType = field.Type.String()
		}
	}
	return idSelection(idField, idMarkerAlias(fieldType))
}

func (d *schemaDecorator) markerSelections(parentType *ast.Definition) ast.SelectionSet {
	selections := ast.SelectionSet{typenameSelection()}

	if parentType == nil {
		return selections
	}

	if idField, ok := d.idFieldByTypename[parentType.Name]; ok {
		selections = append(selections, d.schemaIDSelection(parentType.Name, idField))
	} else if isAbstractDefinition(parentType) {
		for _, possibleType := range d.schema.GetPossibleTypes(parentType) {
			if possibleIDField, ok := d.idFieldByTypename[possibleType.Name]; ok {
				selections = append(selections, &ast.InlineFragment{
					TypeCondition: possibleType.Name,
					SelectionSet:  ast.SelectionSet{d.schemaIDSelection(possibleType.Name, possibleIDField)},
				})
			}
		}
	}

	return selections
}

func (d *schemaDecorator) fieldType(parentType *ast.Definition, field *ast.Field) *ast.Definition {
	if parentType == nil {
		return nil
	}
	if field.Definition != nil && field.ObjectDefinition == parentType {
		return d.schema.Types[field.Definition.Type.Name()]
	}
	definition := parentType.Fields.ForName(field.Name)
	if definition == nil {
		return nil
	}
	return d.schema.Types[definition.Type.Name()]
}

func (d *schemaDecorator) selectionSet(selectionSet ast.SelectionSet, parentType *ast.Definition) ast.SelectionSet {
	markers := d.markerSelections(parentType)
	out := make(ast.SelectionSet, 0, len(selectionSet)+len(markers))

	for _, selection := range selectionSet {
		switch s := selection.(type) {
		case *ast.Field:
			if len(s.SelectionSet) == 0 {
				out = append(out, s)
				continue
			}
			field := *s
			field.SelectionSet = d.selectionSet(s.SelectionSet, d.fieldType(parentType, s))
			out = append(out, &field)
		case *ast.InlineFragment:
			fragmentType := parentType
			if s.TypeCondition != "" {
				fragmentType = d.schema.Types[s.TypeCondition]
			}
			fragment := *s
			fragment.SelectionSet = d.selectionSet(s.SelectionSet, fragmentType)
			out = append(out, &fragment)
		default:
			out = append(out, selection)
		}
	}

	return append(out, markers...)
}

func rootType(schema *ast.Schema, operation ast.Operation) *ast.Definition {
	switch operation {
	case ast.Mutation:
		return schema.Mutation
	case ast.Subscription:
		return schema.Subscription
	default:
		return schema.Query
	}
}

func (d *schemaDecorator) decorateDocument(document *ast.QueryDocument) *ast.QueryDocument {
	if isDecorated(document) || !isQueryOnlyDocument(document) {
		return document
	}

	decorated := &ast.QueryDocument{Position: document.Position, Comment: document.Comment}

	for _, operation := range document.Operations {
		copied := *operation
		copied.SelectionSet = d.selectionSet(operation.SelectionSet, rootType(d.schema, operation.Operation))
		decorated.Operations = append(decorated.Operations, &copied)
	}

	for _, fragment := range document.Fragments {
		copied := *fragment
		copied.SelectionSet = d.selectionSet(fragment.SelectionSet, d.schema.Types[fragment.TypeCondition])
		decorated.Fragments = append(decorated.Fragments, &copied)
	}

	return decorated
}

var clientDecorations weakMemo[ast.QueryDocument, *ast.QueryDocument]

// DecorateClientDocument decorates a document with the marker fields
// Mangrove needs to identify entities, without a schema: mark the id field
// of an entity with `@idField`. Use it when wrapping an executor that runs
// against a remote schema. It is idempotent.
func DecorateClientDocument(document *ast.QueryDocument) *ast.QueryDocument {
	return clientDecorations.get(document, decorateClientDocument)
}

func withoutIDFieldDirective(directives ast.DirectiveList) ast.DirectiveList {
	var out ast.DirectiveList
	for _, directive := range directives {
		if directive.Name != DirectiveIDField {
			out = append(out, directive)
		}
	}
	return out
}

func decorateClientSelectionSet(selectionSet ast.SelectionSet, decorate bool) ast.SelectionSet {
	idField := ""
	for _, selection := range selectionSet {
		if field, ok := selection.(*ast.Field); ok && field.Directives.ForName(DirectiveIDField) != nil {
			idField = field.Name
			break
		}
	}

	out := make(ast.SelectionSet, 0, len(selectionSet)+2)
	for _, selection := range selectionSet {
		switch s := selection.(type) {
		case *ast.Field:
			field := *s
			field.Directives = withoutIDFieldDirective(s.Directives)
			if len(s.SelectionSet) > 0 {
				field.SelectionSet = decorateClientSelectionSet(s.SelectionSet, decorate)
			}
			out = append(out, &field)
		case *ast.InlineFragment:
			fragment := *s
			fragment.Directives = withoutIDFieldDirective(s.Directives)
			fragment.SelectionSet = decorateClientSelectionSet(s.SelectionSet, decorate)
			out = append(out, &fragment)
		case *ast.FragmentSpread:
			spread := *s
			spread.Directives = withoutIDFieldDirective(s.Directives)
			out = append(out, &spread)
		default:
			out = append(out, selection)
		}
	}

	if decorate {
		out = append(out, typenameSelection())
		if idField != "" {
			out = append(out, idSelection(idField, AliasEntityCacheID))
		}
	}

	return out
}

func decorateClientDocument(document *ast.QueryDocument) *ast.QueryDocument {
	if isDecorated(document) {
		return document
	}

	decorate := isQueryOnlyDocument(document)
	decorated := &ast.QueryDocument{Position: document.Position, Comment: document.Comment}

	for _, operation := range document.Operations {
		copied := *operation
		copied.Directives = withoutIDFieldDirective(operation.Directives)
		copied.SelectionSet = decorateClientSelectionSet(operation.SelectionSet, decorate)
		decorated.Operations = append(decorated.Operations, &copied)
	}

	for _, fragment := range document.Fragments {
		copied := *fragment
		copied.Directives = withoutIDFieldDirective(fragment.Directives)
		copied.SelectionSet = decorateClientSelectionSet(fragment.SelectionSet, decorate)
		decorated.Fragments = append(decorated.Fragments, &copied)
	}

	return decorated
}
