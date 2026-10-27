package mangrove

import (
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/validator/core"
)

// NoAliasConventionConflictsRule rejects aliases that could be mistaken for
// Mangrove's cache resolution aliases. Fragments can contribute root fields
// too, so every field alias is checked. Add it to the validation rules of
// servers using Mangrove.
var NoAliasConventionConflictsRule = core.Rule{
	Name: "NoAliasConventionConflicts",
	RuleFunc: func(observers *core.Events, addError core.AddErrFunc) {
		// The walker visits fragments both where they are spread and on
		// their own: report each field once.
		reported := map[*ast.Field]bool{}
		observers.OnField(func(_ *core.Walker, field *ast.Field) {
			if field.Alias != field.Name && strings.HasPrefix(field.Alias, PartialCacheAliasPrefix) && !reported[field] {
				reported[field] = true
				addError(
					core.Message("Field aliases can't start with %s", PartialCacheAliasPrefix),
					core.At(field.Position),
				)
			}
		})
	},
}
