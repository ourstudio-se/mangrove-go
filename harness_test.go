package mangrove_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
	"github.com/ourstudio-se/mangrove-go/memorycache"
	"github.com/ourstudio-se/mangrove-go/rediscache"
	"github.com/redis/go-redis/v9"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// recordingHandler records warnings and errors logged by Mangrove.
type recordingHandler struct {
	mu       sync.Mutex
	warnings []string
	errors   []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	message := record.Message
	record.Attrs(func(attr slog.Attr) bool {
		message += " " + attr.Key + "=" + attr.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case record.Level >= slog.LevelError:
		h.errors = append(h.errors, message)
	case record.Level >= slog.LevelWarn:
		h.warnings = append(h.warnings, message)
	}
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) Warnings() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.warnings...)
}

func (h *recordingHandler) Errors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.errors...)
}

// clock is a settable clock, starting from the real time: Redis keeps real
// time, so absolute expiries must stay in the future.
type clock struct {
	start  time.Time
	offset atomic.Int64
}

func newClock() *clock {
	return &clock{start: time.Now()}
}

func (c *clock) Now() time.Time {
	return c.start.Add(time.Duration(c.offset.Load()))
}

// Set moves the clock to start + offset.
func (c *clock) Set(offset time.Duration) {
	c.offset.Store(int64(offset))
}

// Advance moves the clock forward.
func (c *clock) Advance(d time.Duration) {
	c.offset.Add(int64(d))
}

const directives = `
directive @cacheResolver(keyArg: String) on FIELD_DEFINITION
directive @cacheEntity(ttl: Int) on OBJECT
`

type harnessOptions struct {
	cache      mangrove.Cache
	resolvers  testexec.Resolvers
	typeDefs   string
	ttl        time.Duration
	entityTTLs map[string]time.Duration
	enabled    func(context.Context) bool
	now        func() time.Time
	// noValidate executes documents without validating them, to see what
	// non-validating executors would do.
	noValidate bool
}

type harness struct {
	t        testing.TB
	schema   *testexec.Schema
	m        *mangrove.Mangrove
	strategy *mangrove.LazyStrategy
	logs     *recordingHandler
	execute  mangrove.Executor
}

type executedKey struct{}

// directKey marks reference executions that bypass Mangrove.
type directKey struct{}

// isDirect reports whether a resolver runs for a reference execution, which
// resolvers counting calls should ignore.
func isDirect(ctx context.Context) bool {
	direct, _ := ctx.Value(directKey{}).(bool)
	return direct
}

type executedDocuments struct {
	mu        sync.Mutex
	documents []string
}

func newHarness(t testing.TB, options harnessOptions) *harness {
	t.Helper()
	schema := testexec.MustSchema(directives+options.typeDefs, options.resolvers)
	logs := &recordingHandler{}
	logger := slog.New(logs)
	strategy := mangrove.NewLazyStrategy(mangrove.LazyStrategyOptions{Cache: options.cache, Logger: logger, Now: options.now})

	ttl := options.ttl
	if ttl == 0 {
		ttl = time.Minute
	}

	m, err := mangrove.New(mangrove.Options{
		Strategy:                 strategy,
		TTL:                      ttl,
		EntityTTLs:               options.entityTTLs,
		Schema:                   schema.Schema,
		Session:                  func(context.Context) string { return "session" },
		Enabled:                  options.enabled,
		AwaitWriteBeforeResponse: true,
		IncludeExtensionMetadata: true,
		Logger:                   logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	executor := schema.Executor()
	if options.noValidate {
		executor = func(ctx context.Context, request *mangrove.Request) (*mangrove.Response, error) {
			return schema.Execute(ctx, request.Document, request.OperationName, request.Variables)
		}
	}

	recording := func(ctx context.Context, request *mangrove.Request) (*mangrove.Response, error) {
		if executed, ok := ctx.Value(executedKey{}).(*executedDocuments); ok {
			executed.mu.Lock()
			executed.documents = append(executed.documents, mangrove.PrintDocument(request.Document))
			executed.mu.Unlock()
		}
		return executor(ctx, request)
	}

	return &harness{t: t, schema: schema, m: m, strategy: strategy, logs: logs, execute: m.Wrap(recording)}
}

type runOptions struct {
	operationName string
	variables     map[string]any
	ctx           context.Context
}

type runResult struct {
	// executed holds every document executed against the schema, printed.
	executed []string
	response *mangrove.Response
}

// data returns the response data as JSON.
func (r runResult) data() string {
	return string(r.response.Data)
}

// json returns data and errors, as the response would be serialized
// (without extensions).
func (r runResult) json() string {
	return responseJSON(r.response.Data, r.response.Errors)
}

func (r runResult) extensions() map[string]any {
	cache, _ := r.response.Extensions["cache"].(map[string]any)
	return cache
}

func (r runResult) hit() bool {
	hit, _ := r.extensions()["hit"].(bool)
	return hit
}

func (r runResult) partial() bool {
	partial, _ := r.extensions()["partial"].(bool)
	return partial
}

func responseJSON(data json.RawMessage, errors gqlerror.List) string {
	var out strings.Builder
	out.WriteString(`{`)
	if len(errors) > 0 {
		encoded, _ := json.Marshal(errors)
		out.WriteString(`"errors":`)
		out.Write(encoded)
		out.WriteString(`,`)
	}
	out.WriteString(`"data":`)
	if len(data) == 0 {
		out.WriteString("null")
	} else {
		out.Write(data)
	}
	out.WriteString(`}`)
	return out.String()
}

func (h *harness) run(query string, options ...runOptions) runResult {
	h.t.Helper()
	result, err := h.tryRun(query, options...)
	if err != nil {
		h.t.Fatalf("run failed: %v", err)
	}
	return result
}

func (h *harness) tryRun(query string, options ...runOptions) (runResult, error) {
	var opts runOptions
	if len(options) > 0 {
		opts = options[0]
	}
	ctx := opts.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	executed := &executedDocuments{}
	ctx = context.WithValue(ctx, executedKey{}, executed)

	document, err := h.schema.Parse(query)
	if err != nil {
		return runResult{}, err
	}

	response, err := h.execute(ctx, &mangrove.Request{
		Document:      document,
		OperationName: opts.operationName,
		Variables:     opts.variables,
	})
	if err != nil {
		return runResult{}, err
	}

	executed.mu.Lock()
	defer executed.mu.Unlock()
	return runResult{executed: append([]string(nil), executed.documents...), response: response}, nil
}

// expected executes a query directly, without Mangrove.
func (h *harness) expected(query string, options ...runOptions) string {
	h.t.Helper()
	var opts runOptions
	if len(options) > 0 {
		opts = options[0]
	}
	document := h.schema.MustParse(query)
	response, err := h.schema.Execute(context.WithValue(context.Background(), directKey{}, true), document, opts.operationName, opts.variables)
	if err != nil {
		h.t.Fatal(err)
	}
	return responseJSON(response.Data, response.Errors)
}

// expectFresh runs a query through Mangrove and checks the result against
// direct execution.
func (h *harness) expectFresh(query string, options ...runOptions) runResult {
	h.t.Helper()
	result := h.run(query, options...)
	if got, want := result.json(), h.expected(query, options...); got != want {
		h.t.Fatalf("stale or wrong result\n got: %s\nwant: %s\nexecuted:\n%s", got, want, strings.Join(result.executed, "\n"))
	}
	return result
}

func (h *harness) invalidate(entities ...mangrove.EntityRecord) {
	h.t.Helper()
	if err := h.strategy.InvalidateEntities(context.Background(), entities); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) expectNoErrors() {
	h.t.Helper()
	if errors := h.logs.Errors(); len(errors) > 0 {
		h.t.Fatalf("unexpected errors logged: %v", errors)
	}
}

/*
 * Redis: REDIS_URL, or a redis-server started for the test run.
 */

var (
	redisURL     string
	redisProcess *exec.Cmd
)

func TestMain(m *testing.M) {
	redisURL = os.Getenv("REDIS_URL")
	if redisURL == "" {
		if _, err := exec.LookPath("redis-server"); err == nil {
			port := 20000 + rand.IntN(20000)
			cmd := exec.Command("redis-server", "--port", strconv.Itoa(port), "--save", "", "--appendonly", "no")
			if err := cmd.Start(); err == nil {
				redisProcess = cmd
				redisURL = fmt.Sprintf("redis://127.0.0.1:%d", port)
				client := redis.NewClient(&redis.Options{Addr: fmt.Sprintf("127.0.0.1:%d", port)})
				for i := 0; i < 50; i++ {
					if client.Ping(context.Background()).Err() == nil {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				_ = client.Close()
			}
		}
	}

	code := m.Run()

	if redisProcess != nil {
		_ = redisProcess.Process.Kill()
		_ = redisProcess.Wait()
	}
	os.Exit(code)
}

type backend struct {
	name     string
	newCache func(t testing.TB, now func() time.Time) mangrove.Cache
}

func memoryBackend() backend {
	return backend{name: "memory", newCache: func(_ testing.TB, now func() time.Time) mangrove.Cache {
		return memorycache.New(memorycache.Options{Now: now})
	}}
}

var redisMu sync.Mutex

func newRedisClient(t testing.TB) *redis.Client {
	t.Helper()
	if redisURL == "" {
		t.Skip("redis-server not available")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func redisBackend() backend {
	return backend{name: "redis", newCache: func(t testing.TB, now func() time.Time) mangrove.Cache {
		// Tests using Redis share one database: one at a time.
		redisMu.Lock()
		t.Cleanup(redisMu.Unlock)
		client := newRedisClient(t)
		if err := client.FlushAll(context.Background()).Err(); err != nil {
			t.Fatal(err)
		}
		return rediscache.New(rediscache.Options{Client: client, Now: now})
	}}
}

func backends() []backend {
	return []backend{memoryBackend(), redisBackend()}
}

// forEachBackend runs a test against every cache backend.
func forEachBackend(t *testing.T, test func(t *testing.T, b backend)) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) { test(t, b) })
	}
}
