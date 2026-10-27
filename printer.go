package mangrove

import (
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

/*
 * Prints executable documents. gqlparser's formatter quotes strings the way
 * Go does, which isn't always valid GraphQL (e.g. `\x01`); ids end up in
 * generated documents as string literals, so they must be printed correctly.
 */

// PrintDocument prints an executable document.
func PrintDocument(document *ast.QueryDocument) string {
	p := printer{}
	for i, operation := range document.Operations {
		if i > 0 {
			p.WriteString("\n\n")
		}
		p.operation(operation)
	}
	for i, fragment := range document.Fragments {
		if i > 0 || len(document.Operations) > 0 {
			p.WriteString("\n\n")
		}
		p.fragment(fragment)
	}
	return p.String()
}

type printer struct {
	strings.Builder
	indent int
}

func (p *printer) newline() {
	p.WriteByte('\n')
	for i := 0; i < p.indent; i++ {
		p.WriteString("  ")
	}
}

func (p *printer) operation(operation *ast.OperationDefinition) {
	shorthand := operation.Operation == ast.Query && operation.Name == "" &&
		len(operation.VariableDefinitions) == 0 && len(operation.Directives) == 0
	if !shorthand {
		p.WriteString(string(operation.Operation))
		if operation.Name != "" {
			p.WriteByte(' ')
			p.WriteString(operation.Name)
		}
		if len(operation.VariableDefinitions) > 0 {
			p.WriteByte('(')
			for i, definition := range operation.VariableDefinitions {
				if i > 0 {
					p.WriteString(", ")
				}
				p.WriteByte('$')
				p.WriteString(definition.Variable)
				p.WriteString(": ")
				p.WriteString(definition.Type.String())
				if definition.DefaultValue != nil {
					p.WriteString(" = ")
					p.value(definition.DefaultValue)
				}
				p.directives(definition.Directives)
			}
			p.WriteByte(')')
		}
		p.directives(operation.Directives)
		p.WriteByte(' ')
	}
	p.selectionSet(operation.SelectionSet)
}

func (p *printer) fragment(fragment *ast.FragmentDefinition) {
	p.WriteString("fragment ")
	p.WriteString(fragment.Name)
	p.WriteString(" on ")
	p.WriteString(fragment.TypeCondition)
	p.directives(fragment.Directives)
	p.WriteByte(' ')
	p.selectionSet(fragment.SelectionSet)
}

func (p *printer) selectionSet(selectionSet ast.SelectionSet) {
	p.WriteByte('{')
	p.indent++
	for _, selection := range selectionSet {
		p.newline()
		switch selection := selection.(type) {
		case *ast.Field:
			p.field(selection)
		case *ast.InlineFragment:
			p.WriteString("...")
			if selection.TypeCondition != "" {
				p.WriteString(" on ")
				p.WriteString(selection.TypeCondition)
			}
			p.directives(selection.Directives)
			p.WriteByte(' ')
			p.selectionSet(selection.SelectionSet)
		case *ast.FragmentSpread:
			p.WriteString("...")
			p.WriteString(selection.Name)
			p.directives(selection.Directives)
		}
	}
	p.indent--
	p.newline()
	p.WriteByte('}')
}

func (p *printer) field(field *ast.Field) {
	if field.Alias != "" && field.Alias != field.Name {
		p.WriteString(field.Alias)
		p.WriteString(": ")
	}
	p.WriteString(field.Name)
	if len(field.Arguments) > 0 {
		p.WriteByte('(')
		for i, argument := range field.Arguments {
			if i > 0 {
				p.WriteString(", ")
			}
			p.WriteString(argument.Name)
			p.WriteString(": ")
			p.value(argument.Value)
		}
		p.WriteByte(')')
	}
	p.directives(field.Directives)
	if len(field.SelectionSet) > 0 {
		p.WriteByte(' ')
		p.selectionSet(field.SelectionSet)
	}
}

func (p *printer) directives(directives ast.DirectiveList) {
	for _, directive := range directives {
		p.WriteString(" @")
		p.WriteString(directive.Name)
		if len(directive.Arguments) > 0 {
			p.WriteByte('(')
			for i, argument := range directive.Arguments {
				if i > 0 {
					p.WriteString(", ")
				}
				p.WriteString(argument.Name)
				p.WriteString(": ")
				p.value(argument.Value)
			}
			p.WriteByte(')')
		}
	}
}

func (p *printer) value(value *ast.Value) {
	if value == nil {
		p.WriteString("null")
		return
	}
	switch value.Kind {
	case ast.Variable:
		p.WriteByte('$')
		p.WriteString(value.Raw)
	case ast.StringValue, ast.BlockValue:
		writeGraphQLString(&p.Builder, value.Raw)
	case ast.ListValue:
		p.WriteByte('[')
		for i, child := range value.Children {
			if i > 0 {
				p.WriteString(", ")
			}
			p.value(child.Value)
		}
		p.WriteByte(']')
	case ast.ObjectValue:
		p.WriteByte('{')
		for i, child := range value.Children {
			if i > 0 {
				p.WriteString(", ")
			}
			p.WriteString(child.Name)
			p.WriteString(": ")
			p.value(child.Value)
		}
		p.WriteByte('}')
	default:
		p.WriteString(value.Raw)
	}
}

func writeGraphQLString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[r>>4])
			b.WriteByte(hexDigits[r&0xf])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}
