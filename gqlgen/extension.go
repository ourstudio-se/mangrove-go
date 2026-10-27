// Package gqlgen integrates Mangrove with gqlgen servers as a handler
// extension:
//
//	srv := handler.New(generated.NewExecutableSchema(config))
//	srv.AddTransport(transport.POST{})
//	srv.Use(gqlgen.New(mangrove.Options{Strategy: strategy, TTL: time.Minute}))
//
// Queries are served from the cache. The documents Mangrove generates to
// refresh cached results are executed through the rest of the server's
// operation middleware and field middleware, like any other operation.
package gqlgen

import (
	"context"
	"errors"
	"fmt"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/ourstudio-se/mangrove-go"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	// Registers the standard validation rules.
	_ "github.com/vektah/gqlparser/v2/validator/rules"
)

// Extension is a gqlgen handler extension that caches query results.
type Extension struct {
	options   mangrove.Options
	mangrove  *mangrove.Mangrove
	schema    *ast.Schema
	documents graphql.Cache[*ast.QueryDocument]
}

var (
	_ graphql.HandlerExtension     = (*Extension)(nil)
	_ graphql.OperationInterceptor = (*Extension)(nil)
)

// DocumentCacheSize is the number of generated documents kept parsed and
// validated.
const DocumentCacheSize = 1000

// New creates the extension. options.Schema is taken from the server's
// executable schema.
func New(options mangrove.Options) *Extension {
	return &Extension{options: options, documents: lru.New[*ast.QueryDocument](DocumentCacheSize)}
}

// ExtensionName names the extension.
func (e *Extension) ExtensionName() string {
	return "Mangrove"
}

// Validate configures Mangrove from the server's schema.
func (e *Extension) Validate(schema graphql.ExecutableSchema) error {
	options := e.options
	options.Schema = schema.Schema()
	m, err := mangrove.New(options)
	if err != nil {
		return err
	}
	e.mangrove = m
	e.schema = options.Schema
	return nil
}

// Mangrove returns the configured Mangrove instance, once the extension has
// been added to a server.
func (e *Extension) Mangrove() *mangrove.Mangrove {
	return e.mangrove
}

// InvalidateEntities invalidates entities through the strategy.
func (e *Extension) InvalidateEntities(ctx context.Context, entities ...mangrove.EntityRecord) error {
	return e.options.Strategy.InvalidateEntities(ctx, entities)
}

type generatedKey struct{}

func hasIncrementalDelivery(selectionSet ast.SelectionSet, fragments ast.FragmentDefinitionList, seen map[string]bool) bool {
	for _, selection := range selectionSet {
		switch s := selection.(type) {
		case *ast.Field:
			if s.Directives.ForName("stream") != nil || hasIncrementalDelivery(s.SelectionSet, fragments, seen) {
				return true
			}
		case *ast.InlineFragment:
			if s.Directives.ForName("defer") != nil || hasIncrementalDelivery(s.SelectionSet, fragments, seen) {
				return true
			}
		case *ast.FragmentSpread:
			if s.Directives.ForName("defer") != nil {
				return true
			}
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			if fragment := fragments.ForName(s.Name); fragment != nil && hasIncrementalDelivery(fragment.SelectionSet, fragments, seen) {
				return true
			}
		}
	}
	return false
}

// InterceptOperation serves queries through Mangrove.
func (e *Extension) InterceptOperation(ctx context.Context, next graphql.OperationHandler) graphql.ResponseHandler {
	operationContext := graphql.GetOperationContext(ctx)

	if e.mangrove == nil ||
		ctx.Value(generatedKey{}) != nil ||
		operationContext.Operation == nil ||
		operationContext.Operation.Operation != ast.Query ||
		// Mangrove handles single responses only.
		hasIncrementalDelivery(operationContext.Operation.SelectionSet, operationContext.Doc.Fragments, map[string]bool{}) {
		return next(ctx)
	}

	request := &mangrove.Request{
		Document:       operationContext.Doc,
		DocumentString: operationContext.RawQuery,
		OperationName:  operationContext.OperationName,
		Variables:      operationContext.Variables,
		Extensions:     operationContext.Extensions,
	}

	response, err := e.mangrove.Execute(ctx, e.executor(operationContext, next), request)
	if err != nil {
		var list gqlerror.List
		if !errors.As(err, &list) {
			list = gqlerror.List{gqlerror.WrapIfUnwrapped(err)}
		}
		return graphql.OneShot(&graphql.Response{Errors: list})
	}

	return graphql.OneShot(&graphql.Response{
		Data:       response.Data,
		Errors:     response.Errors,
		Extensions: response.Extensions,
	})
}

func (e *Extension) parse(ctx context.Context, query string) (*ast.QueryDocument, error) {
	if document, ok := e.documents.Get(ctx, query); ok {
		return document, nil
	}
	document, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		return nil, gqlerror.List{gqlerror.WrapIfUnwrapped(err)}
	}
	if errs := validator.ValidateWithRules(e.schema, document, nil); len(errs) > 0 {
		return nil, errs
	}
	e.documents.Add(ctx, query, document)
	return document, nil
}

// executor runs documents through the rest of the operation middleware, as
// operations of their own.
func (e *Extension) executor(original *graphql.OperationContext, next graphql.OperationHandler) mangrove.Executor {
	return func(ctx context.Context, request *mangrove.Request) (*mangrove.Response, error) {
		var handler graphql.ResponseHandler
		innerCtx := ctx

		if request.Document == original.Doc && request.OperationName == original.OperationName {
			// The original operation, as gqlgen prepared it.
			handler = next(ctx)
		} else {
			query := mangrove.PrintDocument(request.Document)
			document, err := e.parse(ctx, query)
			if err != nil {
				return nil, fmt.Errorf("mangrove: generated an invalid document: %w\n%s", err, query)
			}

			operation := document.Operations.ForName(request.OperationName)
			if operation == nil {
				return nil, fmt.Errorf("mangrove: operation %q not found in generated document", request.OperationName)
			}

			operationContext := &graphql.OperationContext{
				RawQuery:               query,
				Variables:              original.Variables,
				OperationName:          request.OperationName,
				Doc:                    document,
				Extensions:             request.Extensions,
				Headers:                original.Headers,
				Operation:              operation,
				DisableIntrospection:   original.DisableIntrospection,
				RecoverFunc:            original.RecoverFunc,
				ResolverMiddleware:     original.ResolverMiddleware,
				RootResolverMiddleware: original.RootResolverMiddleware,
				Stats:                  original.Stats,
			}

			innerCtx = graphql.WithOperationContext(context.WithValue(ctx, generatedKey{}, true), operationContext)
			handler = next(innerCtx)
		}

		response := handler(innerCtx)
		if response == nil {
			return nil, errors.New("mangrove: the operation returned no response")
		}
		if response.HasNext != nil && *response.HasNext {
			return nil, errors.New("mangrove: incremental delivery is not supported")
		}

		return &mangrove.Response{
			Data:       response.Data,
			Errors:     response.Errors,
			Extensions: response.Extensions,
		}, nil
	}
}
