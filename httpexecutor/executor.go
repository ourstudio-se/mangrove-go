// Package httpexecutor executes GraphQL requests against a remote server
// over HTTP, for caching the responses of a remote GraphQL API on the
// client side:
//
//	m, _ := mangrove.New(mangrove.Options{Strategy: strategy, TTL: time.Minute, CacheResolvers: resolvers})
//	execute := m.Wrap(httpexecutor.New("https://example.com/graphql", httpexecutor.Options{}))
//	document := mangrove.DecorateClientDocument(parsed) // ids marked with @idField
//	response, err := execute(ctx, &mangrove.Request{Document: document})
package httpexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// Options configure the executor.
type Options struct {
	// Client defaults to http.DefaultClient.
	Client *http.Client
	// Header returns headers to send with a request, e.g. for
	// authentication.
	Header func(ctx context.Context) http.Header
}

type body struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
	Extensions    map[string]any `json:"extensions,omitempty"`
}

// New returns an executor that POSTs requests to endpoint.
func New(endpoint string, options Options) mangrove.Executor {
	client := options.Client
	if client == nil {
		client = http.DefaultClient
	}

	return func(ctx context.Context, request *mangrove.Request) (*mangrove.Response, error) {
		encoded, err := json.Marshal(body{
			Query:         mangrove.PrintDocument(request.Document),
			OperationName: request.OperationName,
			Variables:     request.Variables,
			Extensions:    request.Extensions,
		})
		if err != nil {
			return nil, err
		}

		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		if options.Header != nil {
			for key, values := range options.Header(ctx) {
				for _, value := range values {
					httpRequest.Header.Add(key, value)
				}
			}
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Accept", "application/graphql-response+json, application/json")

		httpResponse, err := client.Do(httpRequest)
		if err != nil {
			return nil, err
		}
		defer httpResponse.Body.Close()

		data, err := io.ReadAll(httpResponse.Body)
		if err != nil {
			return nil, err
		}

		var response struct {
			Data       json.RawMessage `json:"data"`
			Errors     gqlerror.List   `json:"errors"`
			Extensions map[string]any  `json:"extensions"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, fmt.Errorf("httpexecutor: unexpected response (HTTP %d): %w", httpResponse.StatusCode, err)
		}

		return &mangrove.Response{Data: response.Data, Errors: response.Errors, Extensions: response.Extensions}, nil
	}
}
