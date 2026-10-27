package mangrove_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
)

type obj = map[string]any

// fn adapts a resolver of the parent and arguments.
func fn(f func(parent obj, args obj) any) testexec.ResolverFunc {
	return func(_ context.Context, parent any, args map[string]any) (any, error) {
		p, _ := parent.(map[string]any)
		return f(p, args), nil
	}
}

// fn0 adapts a resolver without parameters.
func fn0(f func() any) testexec.ResolverFunc {
	return func(context.Context, any, map[string]any) (any, error) {
		return f(), nil
	}
}

// fnCtx adapts a resolver that needs the context.
func fnCtx(f func(ctx context.Context, parent obj, args obj) (any, error)) testexec.ResolverFunc {
	return func(ctx context.Context, parent any, args map[string]any) (any, error) {
		p, _ := parent.(map[string]any)
		return f(ctx, p, args)
	}
}

func list[T any](items ...T) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func copyObj(o obj) obj {
	out := obj{}
	for k, v := range o {
		out[k] = v
	}
	return out
}

func decodeAny(t testing.TB, s string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("invalid JSON %q: %v", s, err)
	}
	return out
}

// expectJSON compares JSON values, ignoring key order.
func expectJSON(t testing.TB, got, want string) {
	t.Helper()
	if !reflect.DeepEqual(decodeAny(t, got), decodeAny(t, want)) {
		t.Fatalf("unexpected JSON\n got: %s\nwant: %s", got, want)
	}
}

// expectData compares a result's data with JSON, ignoring key order.
func expectData(t testing.TB, r runResult, want string) {
	t.Helper()
	expectJSON(t, r.data(), want)
}

// path returns a value from a result's data, e.g. path("items", 0, "name").
func (r runResult) path(t testing.TB, steps ...any) any {
	t.Helper()
	value := decodeAny(t, r.data())
	for _, step := range steps {
		switch s := step.(type) {
		case string:
			m, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("no field %q in %v", s, value)
			}
			value = m[s]
		case int:
			l, ok := value.([]any)
			if !ok || s >= len(l) {
				t.Fatalf("no index %d in %v", s, value)
			}
			value = l[s]
		}
	}
	return value
}

func expectEqual(t testing.TB, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func expectContains(t testing.TB, s, substring string) {
	t.Helper()
	if !strings.Contains(s, substring) {
		t.Fatalf("expected %q in:\n%s", substring, s)
	}
}

func expectNotContains(t testing.TB, s, substring string) {
	t.Helper()
	if strings.Contains(s, substring) {
		t.Fatalf("unexpected %q in:\n%s", substring, s)
	}
}

func expectLen[T any](t testing.TB, items []T, n int) {
	t.Helper()
	if len(items) != n {
		t.Fatalf("expected %d items, got %d: %v", n, len(items), items)
	}
}

// countingCache counts writes of results (not set memberships).
type countingCache struct {
	mangrove.Cache
	sets atomic.Int64
}

type countingPipe struct {
	mangrove.CachePipe
	cache *countingCache
}

func (c *countingCache) GetPipe(condition *mangrove.StoreCondition) (mangrove.CachePipe, error) {
	pipe, err := c.Cache.GetPipe(condition)
	if err != nil {
		return nil, err
	}
	return &countingPipe{CachePipe: pipe, cache: c}, nil
}

func (p *countingPipe) Set(ctx context.Context, key, data string, ttl time.Duration) error {
	p.cache.sets.Add(1)
	return p.CachePipe.Set(ctx, key, data, ttl)
}

// gate blocks resolvers until released.
type gate struct {
	started chan struct{}
	release chan struct{}
	once    atomic.Bool
}

func newGate() *gate {
	return &gate{started: make(chan struct{}), release: make(chan struct{})}
}

// wait signals that a resolver reached the gate (once), then blocks until
// the gate is opened.
func (g *gate) wait() {
	if g.once.CompareAndSwap(false, true) {
		close(g.started)
	}
	<-g.release
}

func (g *gate) open() {
	close(g.release)
}

type asyncRun struct {
	done   chan struct{}
	result runResult
	err    error
}

// runAsync runs a query in the background.
func (h *harness) runAsync(query string, options ...runOptions) *asyncRun {
	a := &asyncRun{done: make(chan struct{})}
	go func() {
		defer close(a.done)
		a.result, a.err = h.tryRun(query, options...)
	}()
	return a
}

func (a *asyncRun) wait(t testing.TB) runResult {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a request")
	}
	if a.err != nil {
		t.Fatal(a.err)
	}
	return a.result
}
