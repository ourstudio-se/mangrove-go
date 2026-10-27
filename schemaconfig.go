package mangrove

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
)

// DirectivesSDL declares Mangrove's schema directives.
const DirectivesSDL = `
directive @cacheResolver(keyArg: String) on FIELD_DEFINITION
directive @cacheEntity(ttl: Int) on OBJECT
`

// SchemaConfig is Mangrove's configuration derived from a schema.
type SchemaConfig struct {
	CacheResolvers    CacheResolverMap
	EntityTTLs        map[string]time.Duration
	IDFieldByTypename map[string]string
	// Decorate adds the marker fields Mangrove needs to a document. It is
	// idempotent.
	Decorate func(*ast.QueryDocument) *ast.QueryDocument
}

func isBatchType(t *ast.Type) bool {
	return t.Elem != nil
}

func isLeafDefinition(definition *ast.Definition) bool {
	return definition != nil && (definition.Kind == ast.Scalar || definition.Kind == ast.Enum)
}

func isAbstractDefinition(definition *ast.Definition) bool {
	return definition != nil && (definition.Kind == ast.Interface || definition.Kind == ast.Union)
}

func entityTTLFromObjectType(definition *ast.Definition) (time.Duration, bool) {
	directive := definition.Directives.ForName(DirectiveCacheEntity)
	if directive == nil {
		return 0, false
	}
	argument := directive.Arguments.ForName("ttl")
	if argument == nil || argument.Value == nil || argument.Value.Kind != ast.IntValue {
		return 0, false
	}
	ms, err := strconv.ParseInt(argument.Value.Raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

func keyTypeOf(t *ast.Type) (ResolverKeyType, bool) {
	switch t.Name() {
	case "Int":
		return KeyTypeInt, true
	case "Float":
		return KeyTypeFloat, true
	case "String", "ID":
		return KeyTypeString, true
	default:
		return "", false
	}
}

func isRequiredArgument(argument *ast.ArgumentDefinition) bool {
	return argument.Type.NonNull && argument.DefaultValue == nil
}

func cacheResolversFromQueryRootField(field *ast.FieldDefinition, schema *ast.Schema, logger *slog.Logger) CacheResolverMap {
	annotation := field.Directives.ForName(DirectiveCacheResolver)

	if annotation == nil {
		return nil
	}

	skip := func(reason string) CacheResolverMap {
		logger.Warn(fmt.Sprintf("[Mangrove] Ignoring @%s on Query.%s: %s", DirectiveCacheResolver, field.Name, reason))
		return nil
	}

	var keyArg *ast.ArgumentDefinition

	if annotated := annotation.Arguments.ForName("keyArg"); annotated != nil && annotated.Value != nil && annotated.Value.Kind != ast.NullValue {
		keyArg = field.Arguments.ForName(annotated.Value.Raw)
		if keyArg == nil {
			return skip(fmt.Sprintf("argument %q does not exist", annotated.Value.Raw))
		}
	} else {
		var required []*ast.ArgumentDefinition
		for _, argument := range field.Arguments {
			if isRequiredArgument(argument) {
				required = append(required, argument)
			}
		}
		switch {
		case len(field.Arguments) == 1:
			keyArg = field.Arguments[0]
		case len(required) == 1:
			keyArg = required[0]
		default:
			return skip("the field must have exactly one (required) argument, or specify keyArg")
		}
	}

	for _, argument := range field.Arguments {
		if argument != keyArg && isRequiredArgument(argument) {
			return skip(fmt.Sprintf("argument %q is also required", argument.Name))
		}
	}

	keyType, ok := keyTypeOf(keyArg.Type)

	if !ok {
		return skip(fmt.Sprintf("key argument type %s is not supported (use ID, String, Int or Float)", keyArg.Type.Name()))
	}

	resolver := CacheResolver{
		Batch:     isBatchType(field.Type) && isBatchType(keyArg.Type),
		IDArg:     keyArg.Name,
		RootField: field.Name,
		Type:      keyType,
	}

	namedType := schema.Types[field.Type.Name()]

	if namedType != nil && namedType.Kind == ast.Object {
		return CacheResolverMap{namedType.Name: resolver}
	}

	if isAbstractDefinition(namedType) {
		// e.g. `node(id: ID!): Node`: resolves every possible type.
		resolvers := CacheResolverMap{}
		for _, possibleType := range schema.GetPossibleTypes(namedType) {
			abstract := resolver
			abstract.Abstract = true
			resolvers[possibleType.Name] = abstract
		}
		return resolvers
	}

	return skip("the field must return an object, interface or union type")
}

// WithTypeConditions adds the type conditions that apply to each resolver's
// type (its interfaces and the unions it is a member of), leaving resolvers
// that already declare them alone. Resolvers on root fields returning an
// interface or union are marked as abstract.
func WithTypeConditions(schema *ast.Schema, resolvers CacheResolverMap) CacheResolverMap {
	var unions []*ast.Definition
	for _, definition := range schema.Types {
		if definition.Kind == ast.Union {
			unions = append(unions, definition)
		}
	}

	out := make(CacheResolverMap, len(resolvers))

	for typename, resolver := range resolvers {
		definition := schema.Types[typename]

		if !resolver.Abstract && schema.Query != nil {
			if rootField := schema.Query.Fields.ForName(resolver.RootField); rootField != nil &&
				isAbstractDefinition(schema.Types[rootField.Type.Name()]) {
				resolver.Abstract = true
			}
		}

		if resolver.TypeConditions != nil || definition == nil || definition.Kind != ast.Object {
			out[typename] = resolver
			continue
		}

		typeConditions := append([]string{}, definition.Interfaces...)
		for _, union := range unions {
			for _, member := range union.Types {
				if member == typename {
					typeConditions = append(typeConditions, union.Name)
					break
				}
			}
		}
		resolver.TypeConditions = typeConditions
		out[typename] = resolver
	}

	return out
}

// ConfigureFromSchema derives Mangrove's configuration from a schema: id
// fields, cache resolvers (@cacheResolver) and entity TTLs (@cacheEntity).
// idFields are the names of id fields, in order of preference.
func ConfigureFromSchema(schema *ast.Schema, idFields []string, logger *slog.Logger) SchemaConfig {
	if logger == nil {
		logger = slog.Default()
	}

	entityTTLs := map[string]time.Duration{}
	idFieldByTypename := map[string]string{}

	for typename, definition := range schema.Types {
		if len(typename) >= 2 && typename[:2] == "__" {
			continue
		}

		if definition.Kind != ast.Object && definition.Kind != ast.Interface {
			continue
		}

		if definition.Kind == ast.Object {
			if ttl, ok := entityTTLFromObjectType(definition); ok {
				entityTTLs[typename] = ttl
			}
		}

		for _, idField := range idFields {
			field := definition.Fields.ForName(idField)
			if field == nil || field.Type.Elem != nil || !isLeafDefinition(schema.Types[field.Type.Name()]) {
				continue
			}
			// Ids must be plain scalars that can be selected without
			// arguments.
			selectable := true
			for _, argument := range field.Arguments {
				if isRequiredArgument(argument) {
					selectable = false
					break
				}
			}
			if selectable {
				idFieldByTypename[typename] = idField
				break
			}
		}
	}

	abstractResolvers := CacheResolverMap{}
	objectResolvers := CacheResolverMap{}

	if schema.Query != nil {
		for _, field := range schema.Query.Fields {
			namedType := schema.Types[field.Type.Name()]
			target := abstractResolvers
			if namedType != nil && namedType.Kind == ast.Object {
				target = objectResolvers
			}
			for typename, resolver := range cacheResolversFromQueryRootField(field, schema, logger) {
				target[typename] = resolver
			}
		}
	}

	// Resolvers declared for a concrete type take precedence.
	cacheResolvers := CacheResolverMap{}
	for typename, resolver := range abstractResolvers {
		cacheResolvers[typename] = resolver
	}
	for typename, resolver := range objectResolvers {
		cacheResolvers[typename] = resolver
	}

	decorator := &schemaDecorator{schema: schema, idFieldByTypename: idFieldByTypename}

	return SchemaConfig{
		CacheResolvers:    WithTypeConditions(schema, cacheResolvers),
		EntityTTLs:        entityTTLs,
		IDFieldByTypename: idFieldByTypename,
		Decorate:          decorator.decorate,
	}
}
