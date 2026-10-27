package mangrove_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
)

/*
 * Differential test: random sequences of changes to the data, each followed
 * by the invalidations the caching model requires (the entity whose fields
 * changed, or Query for root fields). After every step, each query served
 * through Mangrove must equal executing it directly.
 *
 * Port of src/__tests__/differential.test.ts. Scale up with e.g.
 * FUZZ_SEEDS=200 FUZZ_STEPS=200 (defaults 6 and 60). Also honors
 * FUZZ_SEED_OFFSET, FUZZ_BACKENDS (extensions of the TS version, same
 * defaults) and FUZZ_NO_VALIDATE.
 */

// ---------------------------------------------------------------- shared

// mulberry32 is the PRNG of the TS fuzzers, with the same sequences.
type mulberry32 struct{ state uint32 }

func newMulberry32(seed int) *mulberry32 {
	return &mulberry32{state: uint32(int64(seed))}
}

func (m *mulberry32) next() float64 {
	m.state += 0x6d2b79f5
	a := m.state
	t := (a ^ (a >> 15)) * (1 | a)
	t = (t + (t^(t>>7))*(61|t)) ^ t
	return float64(t^(t>>14)) / 4294967296
}

type rnd = func() float64

// pickIndex is Math.floor(rand() * n).
func pickIndex(r rnd, n int) int {
	return int(math.Floor(r() * float64(n)))
}

// pickOf is the TS pick: an element, or false for an empty list (still
// drawing a number).
func pickOf[T any](r rnd, list []T) (T, bool) {
	i := pickIndex(r, len(list))
	if i >= len(list) {
		var zero T
		return zero, false
	}
	return list[i], true
}

// filterRand is list.filter(() => rand() < p).
func filterRand[T any](r rnd, list []T, p float64) []T {
	out := []T{}
	for _, item := range list {
		if r() < p {
			out = append(out, item)
		}
	}
	return out
}

// jsRandomSort is V8's Array.prototype.sort with the comparator
// () => rand() - 0.5, drawing the same numbers: for arrays shorter than
// 64, V8's TimSort makes one run (CountAndMakeRun) and extends it with a
// binary insertion sort.
func jsRandomSort[T any](a []T, r rnd) {
	n := len(a)
	if n < 2 {
		return
	}
	if n >= 64 {
		panic("jsRandomSort: only arrays shorter than 64 are supported")
	}
	less := func() bool { return r()-0.5 < 0 }

	// CountAndMakeRun(0, n)
	runLength := 2
	descending := less()
	for idx := 2; idx < n; idx++ {
		order := less()
		if descending {
			if !order {
				break
			}
		} else if order {
			break
		}
		runLength++
	}
	if descending {
		for i, j := 0, runLength-1; i < j; i, j = i+1, j-1 {
			a[i], a[j] = a[j], a[i]
		}
	}

	// BinaryInsertionSort(0, runLength, n)
	for start := runLength; start < n; start++ {
		left, right := 0, start
		pivot := a[start]
		for left < right {
			mid := left + ((right - left) >> 1)
			if less() {
				right = mid
			} else {
				left = mid + 1
			}
		}
		copy(a[left+1:start+1], a[left:start])
		a[left] = pivot
	}
}

// envInt is Number(process.env[name] ?? def).
func envInt(t testing.TB, name string, def int) int {
	value, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return n
}

// envList is (process.env[name] ?? def).split(",").filter(Boolean).
func envList(name, def string) []string {
	value, ok := os.LookupEnv(name)
	if !ok {
		value = def
	}
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envSet(name string) bool {
	return os.Getenv(name) != ""
}

// fuzzBackends returns the backends named by FUZZ_BACKENDS.
func fuzzBackends() []backend {
	names := envList("FUZZ_BACKENDS", "memory,redis")
	var out []backend
	for _, b := range backends() {
		for _, name := range names {
			if b.name == name {
				out = append(out, b)
			}
		}
	}
	return out
}

func fuzzSeeds(t testing.TB, def int) []int {
	n := envInt(t, "FUZZ_SEEDS", def)
	offset := envInt(t, "FUZZ_SEED_OFFSET", 0)
	seeds := make([]int, n)
	for i := range seeds {
		seeds[i] = i + 1 + offset
	}
	return seeds
}

// stringify is JSON.stringify (without HTML escaping), for messages.
func stringify(value any) string {
	var out strings.Builder
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Sprintf("%v", value)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// isArrayIndex reports whether a property key is an array index, which JS
// orders numerically before other keys.
func isArrayIndex(key string) bool {
	if key == "0" {
		return true
	}
	if key == "" || key[0] < '1' || key[0] > '9' {
		return false
	}
	n, err := strconv.ParseUint(key, 10, 64)
	return err == nil && n < math.MaxUint32
}

// omap is a JS object used as a record: Object.keys order is array index
// keys ascending, then the other keys in insertion order.
type omap[V any] struct {
	values map[string]V
	order  []string
}

func newOmap[V any]() *omap[V] {
	return &omap[V]{values: map[string]V{}}
}

func (o *omap[V]) get(key string) (V, bool) {
	v, ok := o.values[key]
	return v, ok
}

func (o *omap[V]) has(key string) bool {
	_, ok := o.values[key]
	return ok
}

func (o *omap[V]) set(key string, value V) {
	if _, ok := o.values[key]; !ok {
		o.order = append(o.order, key)
	}
	o.values[key] = value
}

func (o *omap[V]) del(key string) {
	if _, ok := o.values[key]; !ok {
		return
	}
	delete(o.values, key)
	for i, k := range o.order {
		if k == key {
			o.order = append(o.order[:i:i], o.order[i+1:]...)
			break
		}
	}
}

func (o *omap[V]) keys() []string {
	var indexes, others []string
	for _, k := range o.order {
		if isArrayIndex(k) {
			indexes = append(indexes, k)
		} else {
			others = append(others, k)
		}
	}
	sort.Slice(indexes, func(i, j int) bool {
		a, _ := strconv.ParseUint(indexes[i], 10, 64)
		b, _ := strconv.ParseUint(indexes[j], 10, 64)
		return a < b
	})
	return append(indexes, others...)
}

func (o *omap[V]) clone(cloneValue func(V) V) *omap[V] {
	out := &omap[V]{values: make(map[string]V, len(o.values)), order: append([]string(nil), o.order...)}
	for k, v := range o.values {
		out.values[k] = cloneValue(v)
	}
	return out
}

// inv is an invalidation as the TS fuzzers write it: id is nil (the whole
// type), a string or an int.
type inv struct {
	Typename string `json:"typename"`
	ID       any    `json:"id,omitempty"`
}

func (e inv) MarshalJSON() ([]byte, error) {
	if e.ID == nil {
		return []byte(stringify(struct {
			Typename string `json:"typename"`
		}{e.Typename})), nil
	}
	return []byte(stringify(struct {
		ID       any    `json:"id"`
		Typename string `json:"typename"`
	}{e.ID, e.Typename})), nil
}

func entityRecords(invs []inv) []mangrove.EntityRecord {
	out := make([]mangrove.EntityRecord, len(invs))
	for i, e := range invs {
		switch id := e.ID.(type) {
		case nil:
			out[i] = mangrove.EntityType(e.Typename)
		case string:
			out[i] = mangrove.Entity(e.Typename, id)
		case int:
			out[i] = mangrove.Entity(e.Typename, strconv.Itoa(id))
		default:
			panic(fmt.Sprintf("unexpected id %#v", id))
		}
	}
	return out
}

// useFuzzExecutor makes the harness execute documents the way the TS
// harness does: validated, unless FUZZ_NO_VALIDATE is set, recording each
// executed document for its run. Without validation, documents are printed,
// parsed and annotated with their definitions without running validation
// rules (testexec needs the annotations), like a non-validating server
// would. (The harness's noValidate executes Mangrove's own AST, whose
// generated fields lack the annotations, so their arguments come out nil.)
//
// Each execution holds lock for reading, if given: mutations hold it for
// writing, so that each execution sees one state, as graphql-js executing
// synchronous resolvers does.
func useFuzzExecutor(h *harness, lock *sync.RWMutex) {
	inner := h.schema.Executor()
	if envSet("FUZZ_NO_VALIDATE") {
		inner = func(ctx context.Context, request *mangrove.Request) (*mangrove.Response, error) {
			printed := mangrove.PrintDocument(request.Document)
			document, err := parser.ParseQuery(&ast.Source{Input: printed})
			if err != nil {
				return nil, fmt.Errorf("invalid document: %w\n%s", err, printed)
			}
			validator.Walk(h.schema.Schema, document, &validator.Events{})
			return h.schema.Execute(ctx, document, request.OperationName, request.Variables)
		}
	}
	h.execute = h.m.Wrap(func(ctx context.Context, request *mangrove.Request) (*mangrove.Response, error) {
		if executed, ok := ctx.Value(executedKey{}).(*executedDocuments); ok {
			executed.mu.Lock()
			executed.documents = append(executed.documents, mangrove.PrintDocument(request.Document))
			executed.mu.Unlock()
		}
		if lock != nil {
			lock.RLock()
			defer lock.RUnlock()
		}
		return inner(ctx, request)
	})
}

// ---------------------------------------------------------------- schema

func diff1TypeDefs(withResolvers bool) string {
	d := ""
	if withResolvers {
		d = "@cacheResolver"
	}
	return `
  type Query {
    items: [Item!]!
    featured: Item
    owners: [Owner!]!
    search: [SearchResult!]!
    item(id: ID!): Item ` + d + `
    owner(id: ID!): Owner ` + d + `
  }
  type Item { id: ID! name: String! owner: Owner! tags: [Tag!]! note(prefix: String): String }
  type Owner { id: ID! name: String! items: [Item!]! }
  type Tag { id: ID! label: String! }
  union SearchResult = Item | Owner
`
}

type diff1Query struct {
	query     string
	variables map[string]any
}

var diff1Queries = []diff1Query{
	{query: `{ items { id name owner { id name } } }`},
	{query: `{ featured { id name tags { id label } owner { id name items { id name } } } }`},
	{
		query:     `query Q($p: String) { owners { id name items { id note(prefix: $p) tags { label } } } }`,
		variables: map[string]any{"p": ">"},
	},
	{query: `fragment I on Item { id name owner { name } }
     query S { search { ... on Item { ...I } ... on Owner { id name } } top: featured { ...I } }`},
	{query: `{ items { id tags { id label } } a: featured { id } b: featured { name } }`},
}

// ---------------------------------------------------------------- state

type item1 struct {
	name  string
	owner string
	tags  []string
}

type state1 struct {
	featured  string // "" for null
	items     map[string]*item1
	itemOrder []string
	owners    *omap[string] // id -> name
	search    [][2]string
	tags      *omap[string] // id -> label
	counter   int
}

func diff1InitialState() *state1 {
	owners := newOmap[string]()
	owners.set("o1", "Ann")
	owners.set("o2", "Bob")
	tags := newOmap[string]()
	tags.set("t1", "red")
	tags.set("t2", "blue")
	return &state1{
		featured:  "i1",
		itemOrder: []string{"i1", "i2", "i3"},
		items: map[string]*item1{
			"i1": {name: "one", owner: "o1", tags: []string{"t1"}},
			"i2": {name: "two", owner: "o1", tags: []string{}},
			"i3": {name: "three", owner: "o2", tags: []string{"t1", "t2"}},
		},
		owners:  owners,
		search:  [][2]string{{"Item", "i2"}, {"Owner", "o1"}},
		tags:    tags,
		counter: 100,
	}
}

func (s *state1) next() int {
	n := s.counter
	s.counter++
	return n
}

func diff1Resolvers(s *state1) testexec.Resolvers {
	item := func(id string) any {
		i, ok := s.items[id]
		if !ok {
			return nil
		}
		return obj{"__typename": "Item", "id": id, "name": i.name, "owner": i.owner, "tags": i.tags}
	}
	owner := func(id string) any {
		name, ok := s.owners.get(id)
		if !ok {
			return nil
		}
		return obj{"__typename": "Owner", "id": id, "name": name}
	}
	return testexec.Resolvers{
		"Item": {
			"note": fn(func(i obj, args obj) any {
				prefix, _ := args["prefix"].(string)
				return prefix + i["name"].(string)
			}),
			"owner": fn(func(i obj, _ obj) any { return owner(i["owner"].(string)) }),
			"tags": fn(func(i obj, _ obj) any {
				out := []any{}
				for _, id := range i["tags"].([]string) {
					label, _ := s.tags.get(id)
					out = append(out, obj{"id": id, "label": label})
				}
				return out
			}),
		},
		"Owner": {
			"items": fn(func(o obj, _ obj) any {
				out := []any{}
				for _, id := range s.itemOrder {
					if s.items[id].owner == o["id"] {
						out = append(out, item(id))
					}
				}
				return out
			}),
		},
		"Query": {
			"featured": fn0(func() any {
				if s.featured == "" {
					return nil
				}
				return item(s.featured)
			}),
			"item": fn(func(_ obj, args obj) any { return item(args["id"].(string)) }),
			"items": fn0(func() any {
				out := []any{}
				for _, id := range s.itemOrder {
					out = append(out, item(id))
				}
				return out
			}),
			"owner": fn(func(_ obj, args obj) any { return owner(args["id"].(string)) }),
			"owners": fn0(func() any {
				out := []any{}
				for _, id := range s.owners.keys() {
					out = append(out, owner(id))
				}
				return out
			}),
			"search": fn0(func() any {
				out := []any{}
				for _, entry := range s.search {
					var value any
					if entry[0] == "Item" {
						value = item(entry[1])
					} else {
						value = owner(entry[1])
					}
					if value != nil {
						out = append(out, value)
					}
				}
				return out
			}),
		},
	}
}

// ---------------------------------------------------------------- mutations

// A mutation returns false where the TS returns null (nothing changed, no
// queries run).
type diff1Mutation struct {
	name   string
	mutate func(s *state1, r rnd) ([]inv, bool)
}

var diff1Mutations = []diff1Mutation{
	{"rename item", func(s *state1, r rnd) ([]inv, bool) {
		id, ok := pickOf(r, s.itemOrder)
		if !ok {
			return nil, false
		}
		s.items[id].name = fmt.Sprintf("item%d", s.next())
		return []inv{{"Item", id}}, true
	}},
	{"rename owner", func(s *state1, r rnd) ([]inv, bool) {
		id, _ := pickOf(r, s.owners.keys())
		s.owners.set(id, fmt.Sprintf("owner%d", s.next()))
		return []inv{{"Owner", id}}, true
	}},
	{"relabel tag", func(s *state1, r rnd) ([]inv, bool) {
		id, _ := pickOf(r, s.tags.keys())
		s.tags.set(id, fmt.Sprintf("tag%d", s.next()))
		return []inv{{"Tag", id}}, true
	}},
	{"change item owner", func(s *state1, r rnd) ([]inv, bool) {
		id, ok := pickOf(r, s.itemOrder)
		if !ok {
			return nil, false
		}
		from := s.items[id].owner
		to, _ := pickOf(r, s.owners.keys())
		s.items[id].owner = to
		return []inv{{"Item", id}, {"Owner", from}, {"Owner", to}}, true
	}},
	{"change item tags", func(s *state1, r rnd) ([]inv, bool) {
		id, ok := pickOf(r, s.itemOrder)
		if !ok {
			return nil, false
		}
		s.items[id].tags = filterRand(r, s.tags.keys(), 0.5)
		return []inv{{"Item", id}}, true
	}},
	{"add item", func(s *state1, r rnd) ([]inv, bool) {
		id := fmt.Sprintf("i%d", s.next())
		owner, _ := pickOf(r, s.owners.keys())
		s.items[id] = &item1{name: "new" + id, owner: owner, tags: []string{}}
		at := pickIndex(r, len(s.itemOrder)+1)
		s.itemOrder = append(s.itemOrder[:at:at], append([]string{id}, s.itemOrder[at:]...)...)
		return []inv{{Typename: "Query"}, {"Owner", owner}}, true
	}},
	{"remove item", func(s *state1, r rnd) ([]inv, bool) {
		id, ok := pickOf(r, s.itemOrder)
		if !ok {
			return nil, false
		}
		owner := s.items[id].owner
		order := []string{}
		for _, i := range s.itemOrder {
			if i != id {
				order = append(order, i)
			}
		}
		s.itemOrder = order
		search := [][2]string{}
		for _, entry := range s.search {
			if entry[1] != id {
				search = append(search, entry)
			}
		}
		s.search = search
		if s.featured == id {
			s.featured = ""
			if len(s.itemOrder) > 0 {
				s.featured = s.itemOrder[0]
			}
		}
		delete(s.items, id)
		return []inv{{Typename: "Query"}, {"Owner", owner}, {"Item", id}}, true
	}},
	{"reorder items", func(s *state1, r rnd) ([]inv, bool) {
		jsRandomSort(s.itemOrder, r)
		// Owner.items follows the global order too.
		return []inv{{Typename: "Query"}, {Typename: "Owner"}}, true
	}},
	{"change featured", func(s *state1, r rnd) ([]inv, bool) {
		if r() < 0.2 {
			s.featured = ""
		} else {
			s.featured, _ = pickOf(r, s.itemOrder)
		}
		return []inv{{Typename: "Query"}}, true
	}},
	{"change search", func(s *state1, r rnd) ([]inv, bool) {
		var entries [][2]string
		for _, id := range s.itemOrder {
			entries = append(entries, [2]string{"Item", id})
		}
		for _, id := range s.owners.keys() {
			entries = append(entries, [2]string{"Owner", id})
		}
		s.search = filterRand(r, entries, 0.4)
		jsRandomSort(s.search, r)
		return []inv{{Typename: "Query"}}, true
	}},
	{"nothing", func(*state1, rnd) ([]inv, bool) { return []inv{}, true }},
}

// ---------------------------------------------------------------- runner

func TestDifferential(t *testing.T) {
	seeds := fuzzSeeds(t, 6)
	steps := envInt(t, "FUZZ_STEPS", 60)

	for _, b := range fuzzBackends() {
		t.Run(b.name, func(t *testing.T) {
			for _, withResolvers := range []bool{true, false} {
				for _, seed := range seeds {
					t.Run(fmt.Sprintf("seed=%d,resolvers=%v", seed, withResolvers), func(t *testing.T) {
						runDiff1Seed(t, b, withResolvers, seed, steps)
					})
				}
			}
		})
	}
}

func runDiff1Seed(t *testing.T, b backend, withResolvers bool, seed, steps int) {
	rand := newMulberry32(seed).next
	s := diff1InitialState()
	h := newHarness(t, harnessOptions{
		cache:     b.newCache(t, nil),
		resolvers: diff1Resolvers(s),
		typeDefs:  diff1TypeDefs(withResolvers),
	})
	useFuzzExecutor(h, nil)
	var history []string

	for step := 0; step < steps; step++ {
		m, _ := pickOf(rand, diff1Mutations)
		invalidations, ok := m.mutate(s, rand)
		if !ok {
			continue
		}
		history = append(history, m.name+" -> "+stringify(invalidations))
		if len(invalidations) > 0 {
			h.invalidate(entityRecords(invalidations)...)
		}

		for _, q := range diff1Queries {
			// Not every query runs every step, so cache states diverge.
			if rand() < 0.3 {
				continue
			}
			options := runOptions{variables: q.variables}
			expected := h.expected(q.query, options)
			result := h.run(q.query, options)
			if got := result.json(); got != expected {
				t.Fatal(strings.Join(append([]string{
					fmt.Sprintf("Mismatch for seed %d at step %d running %s", seed, step, q.query),
					"expected " + expected,
					"received " + got,
					"executed:",
				}, append(append(result.executed, "history:"), history...)...), "\n"))
			}
		}
	}

	h.expectNoErrors()
}
