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
// Response middleware registered before the extension sees the final
// response.
package gqlgen

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/ourstudio-se/mangrove-go"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	"github.com/vektah/gqlparser/v2/validator/rules"
)

// Extension is a gqlgen handler extension that caches query results.
type Extension struct {
	// ValidationRules validate the documents Mangrove generates. Defaults to
	// the standard rules. Set it to the server's rules if they differ.
	ValidationRules *rules.Rules

	options   mangrove.Options
	mangrove  *mangrove.Mangrove
	schema    *ast.Schema
	documents graphql.Cache[*ast.QueryDocument]
}

var (
	_ graphql.HandlerExtension     = (*Extension)(nil)
	_ graphql.OperationInterceptor = (*Extension)(nil)
	_ graphql.ResponseInterceptor  = (*Extension)(nil)
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

type pendingKey struct{}

// pending is a query that Mangrove serves once its response is requested.
type pending struct {
	operationContext *graphql.OperationContext
	next             graphql.OperationHandler
}

// InterceptOperation marks queries to be served by Mangrove.
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

	return next(context.WithValue(ctx, pendingKey{}, &pending{operationContext: operationContext, next: next}))
}

// InterceptResponse serves marked queries through Mangrove, in place of
// executing them. Response middleware registered before the extension sees
// the final response (and Mangrove's errors pass through the server's error
// presenter); register the extension after other response middleware.
func (e *Extension) InterceptResponse(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
	p, ok := ctx.Value(pendingKey{}).(*pending)
	if !ok || ctx.Value(generatedKey{}) != nil {
		return next(ctx)
	}

	request := &mangrove.Request{
		Document:       p.operationContext.Doc,
		DocumentString: p.operationContext.RawQuery,
		OperationName:  p.operationContext.OperationName,
		Variables:      p.operationContext.Variables,
		Extensions:     p.operationContext.Extensions,
	}

	response, err := e.mangrove.Execute(ctx, e.executor(p, next), request)
	if err != nil {
		var list gqlerror.List
		if errors.As(err, &list) {
			for _, item := range list {
				graphql.AddError(ctx, item)
			}
		} else {
			graphql.AddError(ctx, err)
		}
		return &graphql.Response{Errors: graphql.GetErrors(ctx), Extensions: graphql.GetExtensions(ctx)}
	}

	extensions := graphql.GetExtensions(ctx)
	if len(response.Extensions) > 0 {
		merged := make(map[string]any, len(extensions)+len(response.Extensions))
		for key, value := range response.Extensions {
			merged[key] = value
		}
		for key, value := range extensions {
			merged[key] = value
		}
		extensions = merged
	}

	return &graphql.Response{
		Data:       response.Data,
		Errors:     append(response.Errors, graphql.GetErrors(ctx)...),
		Extensions: extensions,
	}
}

func (e *Extension) parse(ctx context.Context, query string) (*ast.QueryDocument, error) {
	if document, ok := e.documents.Get(ctx, query); ok {
		return document, nil
	}
	document, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		return nil, gqlerror.List{gqlerror.WrapIfUnwrapped(err)}
	}
	if errs := validator.ValidateWithRules(e.schema, document, e.ValidationRules); len(errs) > 0 {
		return nil, errs
	}
	e.documents.Add(ctx, query, document)
	return document, nil
}

// executor runs documents through the rest of the operation middleware, as
// operations of their own.
func (e *Extension) executor(p *pending, original graphql.ResponseHandler) mangrove.Executor {
	var usedOriginal atomic.Bool
	return func(ctx context.Context, request *mangrove.Request) (*mangrove.Response, error) {
		var handler graphql.ResponseHandler
		innerCtx := ctx

		if request.Document == p.operationContext.Doc && request.OperationName == p.operationContext.OperationName &&
			usedOriginal.CompareAndSwap(false, true) {
			// The original operation, as gqlgen prepared it.
			handler = original
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

			original := p.operationContext
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
			handler = p.next(innerCtx)
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
