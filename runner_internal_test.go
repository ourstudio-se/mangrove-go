package mangrove

import (
	"context"

	"github.com/vektah/gqlparser/v2/ast"
)

// The runner test (ported from src/runner.test.ts) executes against a schema
// through internal/testexec, which imports this package: an internal test
// importing it would be an import cycle. The test lives in
// runner_external_test.go (package mangrove_test) and reaches the unexported
// layeredCacheExecute through this test-only export.

// LayeredCacheExecuteForTest runs layeredCacheExecute without cached
// snapshots or variable definitions (as the TS test does) and returns the
// link queries it executed.
func LayeredCacheExecuteForTest(
	ctx context.Context,
	next Executor,
	resolvers CacheResolverMap,
	linkSelections map[string]ast.SelectionSet,
	originalOperationName string,
	partialQuery *ast.QueryDocument,
) ([]*ast.QueryDocument, error) {
	layered, err := layeredCacheExecute(
		ctx,
		bindExecutor(next, &Request{}),
		resolvers,
		linkSelections,
		newEntitySnapshots(nil),
		originalOperationName,
		partialQuery,
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}
	return layered.linkQueries, nil
}
