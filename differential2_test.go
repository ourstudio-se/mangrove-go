package mangrove_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ourstudio-se/mangrove-go/internal/testexec"
)

/*
 * Aggressive differential test: Int ids, ids with special characters, batch
 * and abstract cache resolvers, interfaces/unions, deep chains, value
 * objects, nested lists, aliases/args, @include/@skip, multi-operation
 * documents, entity TTLs, type-wide invalidation and concurrency.
 *
 * Port of src/__tests__/differential2.test.ts.
 *
 * Env: FUZZ_SEEDS (1; 3 in the TS), FUZZ_STEPS (25; 40 in the TS),
 * FUZZ_SEED_OFFSET (0),
 * FUZZ_BACKENDS=memory,redis, FUZZ_MODES=seq,conc,ttl,
 * FUZZ_VARIANTS=0,1,2,3, FUZZ_NO_DELETE, FUZZ_NO_VALIDATE, FUZZ_MAG_ID
 * (type of Magazine.id, Int!), FUZZ_STATS (file to append stats to),
 * FUZZ_VERBOSE (log stats; use -v), FUZZ_EXAMPLES (file for an example of
 * each kind of warning), FUZZ_TRACE (file for a trace of a failure).
 */

// ---------------------------------------------------------------- schema

/*
 * Variants:
 * 0: node(id) abstract single, books(ids) batch for Book, library(id) Int
 * 1: nodes(ids) abstract batch, book(id) single for Book, library(id)
 * 2: no cache resolvers
 * 3: node(id) only (Book + Author via abstract), no library resolver
 */
func diff2TypeDefs(variant int) string {
	d := func(on bool, arg string) string {
		if !on {
			return ""
		}
		if arg != "" {
			return `@cacheResolver(keyArg: "` + arg + `")`
		}
		return "@cacheResolver"
	}
	magID := os.Getenv("FUZZ_MAG_ID")
	if magID == "" {
		magID = "Int!"
	}
	return `
  interface Node { id: ID! }
  interface Named { name: String! }
  union Media = Book | Magazine

  type Query {
    library(id: Int!, verbose: Boolean): Library ` + d(variant == 0 || variant == 1, "id") + `
    libraries: [Library!]!
    node(id: ID!): Node ` + d(variant == 0 || variant == 3, "") + `
    nodes(ids: [ID!]!): [Node]! ` + d(variant == 1, "") + `
    book(id: ID!): Book ` + d(variant == 1, "") + `
    books(ids: [ID!]!): [Book]! ` + d(variant == 0, "") + `
    author(id: ID!): Author
    featured: Media
    search(term: String): [Media]
    grid: [[Book]]
    named: [Named!]!
    viewer: Viewer!
  }
  type Viewer { favorites: [Book]! home: Library }
  type Library { id: Int! name: String! shelves: [Shelf!]! topAuthor: Author stats: Stats! }
  type Stats { count: Int! best: Book }
  type Shelf implements Named { id: ID! name: String! books: [Book]! grid: [[Book]]! library: Library }
  type Book implements Node & Named {
    id: ID! name: String! title(upper: Boolean): String! author: Author sequel: Book
    edition: Edition related(limit: Int): [Book!]!
  }
  type Edition { year: Int! publisher: Publisher }
  type Publisher { id: ID! name: String! }
  type Author implements Node & Named { id: ID! name: String! books(limit: Int): [Book!]! mentor: Author }
  type Magazine implements Named { id: ` + magID + ` name: String! issue: Int! cover: Book }
`
}

// ---------------------------------------------------------------- state

// Book, author and publisher references use "" for null (no such id
// exists); library references use nullLib.
const nullLib = math.MinInt

type ref2 struct {
	typename string
	id       string // Book, Author, Shelf
	n        int    // Magazine
}

func (r *ref2) MarshalJSON() ([]byte, error) {
	if r.typename == "Magazine" {
		return []byte(stringify([]any{r.typename, r.n})), nil
	}
	return []byte(stringify([]any{r.typename, r.id})), nil
}

type author2 struct {
	mentor string
	name   string
}

type edition2 struct {
	publisher string
	year      int
}

type book2 struct {
	author  string
	edition *edition2
	name    string
	related []string
	sequel  string
	title   string
}

type library2 struct {
	best      string
	count     int
	name      string
	shelves   []string
	topAuthor string
}

type magazine2 struct {
	cover string
	issue int
	name  string
}

type shelf2 struct {
	books   []string
	grid    [][]string
	library int
	name    string
}

type state2 struct {
	authors      *omap[*author2]
	bookOrder    []string
	books        *omap[*book2]
	featured     *ref2
	grid         [][]string
	gridNull     bool
	libraries    *omap[*library2] // keyed by strconv.Itoa(id)
	libraryOrder []int
	magazines    *omap[*magazine2] // keyed by strconv.Itoa(id)
	named        []*ref2
	publishers   *omap[string] // id -> name
	search       []*ref2
	searchNull   bool
	shelves      *omap[*shelf2]
	favorites    []string
	home         int
}

var (
	diff2BookIDs = []string{
		"b1", "b.2", "b:3", "b\"4", "b@5", "b#6", "b>7", "bé8", "b💥9", "b 10",
		"b\\11", "b/12", "b{13}", "b$14", "b%15", "b'16", "b,17", "b`18", "7", "0",
	}
	diff2AuthorIDs    = []string{"a1", "a.2", "a:3", "a\"4", "a@#>5", "aü6", "a\n7"}
	diff2ShelfIDs     = []string{"b1", "s:1", "s.2", "s'3", "", "0", "a1"}
	diff2PublisherIDs = []string{"p1", "b1", "p:2", "1"}
	diff2LibraryIDs   = []int{0, 1, 2, -5}
	diff2MagazineIDs  = []int{0, 1, 7}
)

const diff2TTLEntity = 100 * time.Millisecond

// withNull appends a null book reference.
func withNull(ids []string, n int) []string {
	out := append([]string(nil), ids...)
	for i := 0; i < n; i++ {
		out = append(out, "")
	}
	return out
}

func cloneGrid(grid [][]string) [][]string {
	if grid == nil {
		return nil
	}
	out := make([][]string, len(grid))
	for i, row := range grid {
		out[i] = append([]string{}, row...)
	}
	return out
}

func cloneRefs(refs []*ref2) []*ref2 {
	if refs == nil {
		return nil
	}
	out := make([]*ref2, len(refs))
	for i, r := range refs {
		if r != nil {
			c := *r
			out[i] = &c
		}
	}
	return out
}

func cloneBook(b *book2) *book2 {
	c := *b
	if b.edition != nil {
		e := *b.edition
		c.edition = &e
	}
	c.related = append([]string{}, b.related...)
	return &c
}

func cloneLibrary(l *library2) *library2 {
	c := *l
	c.shelves = append([]string{}, l.shelves...)
	return &c
}

func (s *state2) clone() *state2 {
	c := *s
	c.authors = s.authors.clone(func(a *author2) *author2 { x := *a; return &x })
	c.bookOrder = append([]string{}, s.bookOrder...)
	c.books = s.books.clone(cloneBook)
	if s.featured != nil {
		f := *s.featured
		c.featured = &f
	}
	c.grid = cloneGrid(s.grid)
	c.libraries = s.libraries.clone(cloneLibrary)
	c.libraryOrder = append([]int{}, s.libraryOrder...)
	c.magazines = s.magazines.clone(func(m *magazine2) *magazine2 { x := *m; return &x })
	c.named = cloneRefs(s.named)
	c.publishers = s.publishers.clone(func(n string) string { return n })
	c.search = cloneRefs(s.search)
	c.shelves = s.shelves.clone(func(sh *shelf2) *shelf2 {
		x := *sh
		x.books = append([]string{}, sh.books...)
		x.grid = cloneGrid(sh.grid)
		return &x
	})
	c.favorites = append([]string{}, s.favorites...)
	return &c
}

// dump returns the state as JSON, for traces.
func (s *state2) dump() string {
	nullable := func(id string) any {
		if id == "" {
			return nil
		}
		return id
	}
	nullables := func(ids []string) []any {
		out := []any{}
		for _, id := range ids {
			out = append(out, nullable(id))
		}
		return out
	}
	lib := func(id int) any {
		if id == nullLib {
			return nil
		}
		return id
	}
	out := map[string]any{}
	authors := map[string]any{}
	for _, k := range s.authors.keys() {
		a, _ := s.authors.get(k)
		authors[k] = map[string]any{"mentor": nullable(a.mentor), "name": a.name}
	}
	out["authors"] = authors
	out["bookOrder"] = s.bookOrder
	books := map[string]any{}
	for _, k := range s.books.keys() {
		b, _ := s.books.get(k)
		var edition any
		if b.edition != nil {
			edition = map[string]any{"publisher": nullable(b.edition.publisher), "year": b.edition.year}
		}
		books[k] = map[string]any{
			"author": nullable(b.author), "edition": edition, "name": b.name,
			"related": b.related, "sequel": nullable(b.sequel), "title": b.title,
		}
	}
	out["books"] = books
	out["featured"] = s.featured
	if s.gridNull {
		out["grid"] = nil
	} else {
		rows := []any{}
		for _, row := range s.grid {
			rows = append(rows, nullables(row))
		}
		out["grid"] = rows
	}
	libraries := map[string]any{}
	for _, k := range s.libraries.keys() {
		l, _ := s.libraries.get(k)
		libraries[k] = map[string]any{
			"best": nullable(l.best), "count": l.count, "name": l.name,
			"shelves": l.shelves, "topAuthor": nullable(l.topAuthor),
		}
	}
	out["libraries"] = libraries
	out["libraryOrder"] = s.libraryOrder
	magazines := map[string]any{}
	for _, k := range s.magazines.keys() {
		m, _ := s.magazines.get(k)
		magazines[k] = map[string]any{"cover": nullable(m.cover), "issue": m.issue, "name": m.name}
	}
	out["magazines"] = magazines
	out["named"] = s.named
	publishers := map[string]any{}
	for _, k := range s.publishers.keys() {
		name, _ := s.publishers.get(k)
		publishers[k] = map[string]any{"name": name}
	}
	out["publishers"] = publishers
	if s.searchNull {
		out["search"] = nil
	} else {
		out["search"] = s.search
	}
	shelves := map[string]any{}
	for _, k := range s.shelves.keys() {
		sh, _ := s.shelves.get(k)
		grid := []any{}
		for _, row := range sh.grid {
			grid = append(grid, nullables(row))
		}
		shelves[k] = map[string]any{"books": nullables(sh.books), "grid": grid, "library": lib(sh.library), "name": sh.name}
	}
	out["shelves"] = shelves
	out["viewer"] = map[string]any{"favorites": nullables(s.favorites), "home": lib(s.home)}
	encoded, _ := json.MarshalIndent(out, "", " ")
	return string(encoded)
}

// fuzz2 is the state of one run: the data, deleted entities and the
// counter for fresh names.
type fuzz2 struct {
	s       *state2
	counter int
	deleted struct {
		authors   *omap[*author2]
		books     *omap[*book2]
		libraries *omap[*library2]
	}
}

func (f *fuzz2) next() int {
	n := f.counter
	f.counter++
	return n
}

func (f *fuzz2) word(prefix string) string {
	return prefix + strconv.Itoa(f.next())
}

func diff2InitialState(f *fuzz2, rand rnd) *state2 {
	pickN := func(list []string, p float64) []string { return filterRand(rand, list, p) }
	pickOne := func(list []string) string { v, _ := pickOf(rand, list); return v }
	pickOrNull := func(list []string, p float64) string {
		if rand() < p {
			return ""
		}
		return pickOne(list)
	}
	pickLibOrNull := func(p float64) int {
		if rand() < p {
			return nullLib
		}
		v, _ := pickOf(rand, diff2LibraryIDs)
		return v
	}

	s := &state2{
		authors:    newOmap[*author2](),
		bookOrder:  append([]string{}, diff2BookIDs...),
		books:      newOmap[*book2](),
		featured:   &ref2{typename: "Book", id: "b1"},
		gridNull:   true,
		libraries:  newOmap[*library2](),
		magazines:  newOmap[*magazine2](),
		publishers: newOmap[string](),
		searchNull: true,
		shelves:    newOmap[*shelf2](),
		favorites:  []string{},
		home:       1,
	}
	s.libraryOrder = append([]int{}, diff2LibraryIDs...)
	for _, id := range diff2AuthorIDs {
		s.authors.set(id, &author2{name: f.word("auth")})
	}
	for _, id := range diff2AuthorIDs {
		a, _ := s.authors.get(id)
		a.mentor = pickOrNull(diff2AuthorIDs, 0.3)
	}
	for _, id := range diff2PublisherIDs {
		s.publishers.set(id, f.word("pub"))
	}
	for _, id := range diff2BookIDs {
		b := &book2{}
		b.author = pickOrNull(diff2AuthorIDs, 0.2)
		if rand() >= 0.2 {
			b.edition = &edition2{publisher: pickOrNull(diff2PublisherIDs, 0.2), year: 1900 + f.next()}
		}
		b.name = f.word("book")
		b.related = pickN(diff2BookIDs, 0.15)
		b.sequel = pickOrNull(diff2BookIDs, 0.5)
		b.title = f.word("title")
		s.books.set(id, b)
	}
	for _, id := range diff2ShelfIDs {
		sh := &shelf2{}
		sh.books = pickN(withNull(diff2BookIDs, 1), 0.25)
		sh.grid = [][]string{pickN(withNull(diff2BookIDs, 1), 0.15), {}, pickN(diff2BookIDs, 0.1)}
		sh.library = pickLibOrNull(0.2)
		sh.name = f.word("shelf")
		s.shelves.set(id, sh)
	}
	for _, id := range diff2LibraryIDs {
		l := &library2{}
		l.best = pickOrNull(diff2BookIDs, 0.2)
		l.count = f.next()
		l.name = f.word("lib")
		l.shelves = pickN(diff2ShelfIDs, 0.4)
		l.topAuthor = pickOrNull(diff2AuthorIDs, 0.2)
		s.libraries.set(strconv.Itoa(id), l)
	}
	for _, id := range diff2MagazineIDs {
		m := &magazine2{}
		m.cover = pickOrNull(diff2BookIDs, 0.2)
		m.issue = f.next()
		m.name = f.word("mag")
		s.magazines.set(strconv.Itoa(id), m)
	}
	s.grid = [][]string{pickN(withNull(diff2BookIDs, 1), 0.2), pickN(diff2BookIDs, 0.2)}
	s.gridNull = false
	s.search = []*ref2{
		{typename: "Book", id: "b.2"},
		nil,
		{typename: "Magazine", n: 0},
		{typename: "Book", id: "gone"},
		{typename: "Magazine", n: 7},
	}
	s.searchNull = false
	s.named = []*ref2{
		{typename: "Book", id: "b:3"},
		{typename: "Author", id: "a.2"},
		{typename: "Magazine", n: 1},
		{typename: "Shelf", id: "s:1"},
		{typename: "Shelf", id: ""},
	}
	s.favorites = pickN(withNull(diff2BookIDs, 1), 0.3)
	return s
}

// ---------------------------------------------------------------- resolvers

// stateKey2 holds the state a direct (reference) execution runs against.
// Executions through Mangrove use the live state, under a read lock.
type stateKey2 struct{}

func diff2Resolvers(f *fuzz2) testexec.Resolvers {
	st := func(ctx context.Context) *state2 {
		if s, ok := ctx.Value(stateKey2{}).(*state2); ok {
			return s
		}
		return f.s
	}
	book := func(s *state2, id string) any {
		if id == "" {
			return nil
		}
		b, ok := s.books.get(id)
		if !ok {
			return nil
		}
		return obj{
			"__typename": "Book", "id": id, "author": b.author, "edition": b.edition, "name": b.name,
			"related": b.related, "sequel": b.sequel, "title": b.title,
		}
	}
	author := func(s *state2, id string) any {
		if id == "" {
			return nil
		}
		a, ok := s.authors.get(id)
		if !ok {
			return nil
		}
		return obj{"__typename": "Author", "id": id, "mentor": a.mentor, "name": a.name}
	}
	library := func(s *state2, id int) any {
		if id == nullLib {
			return nil
		}
		l, ok := s.libraries.get(strconv.Itoa(id))
		if !ok {
			return nil
		}
		return obj{
			"__typename": "Library", "id": id, "best": l.best, "count": l.count, "name": l.name,
			"shelves": l.shelves, "topAuthor": l.topAuthor,
		}
	}
	shelf := func(s *state2, id string) any {
		sh, ok := s.shelves.get(id)
		if !ok {
			return nil
		}
		return obj{"__typename": "Shelf", "id": id, "books": sh.books, "grid": sh.grid, "library": sh.library, "name": sh.name}
	}
	magazine := func(s *state2, id int) any {
		m, ok := s.magazines.get(strconv.Itoa(id))
		if !ok {
			return nil
		}
		return obj{"__typename": "Magazine", "id": id, "cover": m.cover, "issue": m.issue, "name": m.name}
	}
	byRef := func(s *state2, r *ref2) any {
		if r == nil {
			return nil
		}
		switch r.typename {
		case "Book":
			return book(s, r.id)
		case "Author":
			return author(s, r.id)
		case "Magazine":
			return magazine(s, r.n)
		case "Shelf":
			return shelf(s, r.id)
		}
		return nil
	}
	node := func(s *state2, id string) any {
		if b := book(s, id); b != nil {
			return b
		}
		return author(s, id)
	}
	books := func(s *state2, ids []string) []any {
		out := make([]any, len(ids))
		for i, id := range ids {
			out[i] = book(s, id)
		}
		return out
	}
	nonNull := func(values []any) []any {
		out := []any{}
		for _, v := range values {
			if v != nil {
				out = append(out, v)
			}
		}
		return out
	}
	limit := func(values []any, n any) []any {
		if n == nil {
			return values
		}
		k := max(0, asInt(n))
		if k > len(values) {
			k = len(values)
		}
		return values[:k]
	}
	ids := func(args obj) []string {
		var out []string
		for _, id := range args["ids"].([]any) {
			out = append(out, id.(string))
		}
		return out
	}
	r := func(resolve func(s *state2, parent obj, args obj) any) testexec.ResolverFunc {
		return fnCtx(func(ctx context.Context, parent obj, args obj) (any, error) {
			return resolve(st(ctx), parent, args), nil
		})
	}

	return testexec.Resolvers{
		"Author": {
			"books": r(func(s *state2, a obj, args obj) any {
				var out []any
				for _, id := range s.bookOrder {
					if b, ok := s.books.get(id); ok && b.author == a["id"] {
						out = append(out, book(s, id))
					}
				}
				return limit(nonNull(out), args["limit"])
			}),
			"mentor": r(func(s *state2, a obj, _ obj) any { return author(s, a["mentor"].(string)) }),
		},
		"Book": {
			"author": r(func(s *state2, b obj, _ obj) any { return author(s, b["author"].(string)) }),
			"edition": r(func(_ *state2, b obj, _ obj) any {
				e := b["edition"].(*edition2)
				if e == nil {
					return nil
				}
				return obj{"publisher": e.publisher, "year": e.year}
			}),
			"related": r(func(s *state2, b obj, args obj) any {
				return limit(nonNull(books(s, b["related"].([]string))), args["limit"])
			}),
			"sequel": r(func(s *state2, b obj, _ obj) any { return book(s, b["sequel"].(string)) }),
			"title": r(func(_ *state2, b obj, args obj) any {
				if upper, _ := args["upper"].(bool); upper {
					return strings.ToUpper(b["title"].(string))
				}
				return b["title"]
			}),
		},
		"Edition": {
			"publisher": r(func(s *state2, e obj, _ obj) any {
				id := e["publisher"].(string)
				if id == "" {
					return nil
				}
				name, ok := s.publishers.get(id)
				if !ok {
					return nil
				}
				return obj{"id": id, "name": name}
			}),
		},
		"Library": {
			"shelves": r(func(s *state2, l obj, _ obj) any {
				var out []any
				for _, id := range l["shelves"].([]string) {
					out = append(out, shelf(s, id))
				}
				return nonNull(out)
			}),
			"stats":     r(func(_ *state2, l obj, _ obj) any { return obj{"best": l["best"], "count": l["count"]} }),
			"topAuthor": r(func(s *state2, l obj, _ obj) any { return author(s, l["topAuthor"].(string)) }),
		},
		"Magazine": {
			"cover": r(func(s *state2, m obj, _ obj) any { return book(s, m["cover"].(string)) }),
		},
		"Query": {
			"author":   r(func(s *state2, _ obj, args obj) any { return author(s, args["id"].(string)) }),
			"book":     r(func(s *state2, _ obj, args obj) any { return book(s, args["id"].(string)) }),
			"books":    r(func(s *state2, _ obj, args obj) any { return books(s, ids(args)) }),
			"featured": r(func(s *state2, _ obj, _ obj) any { return byRef(s, s.featured) }),
			"grid": r(func(s *state2, _ obj, _ obj) any {
				if s.gridNull {
					return nil
				}
				out := []any{}
				for _, row := range s.grid {
					out = append(out, books(s, row))
				}
				return out
			}),
			"libraries": r(func(s *state2, _ obj, _ obj) any {
				var out []any
				for _, id := range s.libraryOrder {
					out = append(out, library(s, id))
				}
				return nonNull(out)
			}),
			"library": r(func(s *state2, _ obj, args obj) any { return library(s, asInt(args["id"])) }),
			"named": r(func(s *state2, _ obj, _ obj) any {
				var out []any
				for _, ref := range s.named {
					out = append(out, byRef(s, ref))
				}
				return nonNull(out)
			}),
			"node": r(func(s *state2, _ obj, args obj) any { return node(s, args["id"].(string)) }),
			"nodes": r(func(s *state2, _ obj, args obj) any {
				out := []any{}
				for _, id := range ids(args) {
					out = append(out, node(s, id))
				}
				return out
			}),
			"search": r(func(s *state2, _ obj, _ obj) any {
				if s.searchNull {
					return nil
				}
				out := []any{}
				for _, ref := range s.search {
					out = append(out, byRef(s, ref))
				}
				return out
			}),
			"viewer": r(func(s *state2, _ obj, _ obj) any {
				return obj{"favorites": s.favorites, "home": s.home}
			}),
		},
		"Shelf": {
			"books": r(func(s *state2, sh obj, _ obj) any { return books(s, sh["books"].([]string)) }),
			"grid": r(func(s *state2, sh obj, _ obj) any {
				out := []any{}
				for _, row := range sh["grid"].([][]string) {
					out = append(out, books(s, row))
				}
				return out
			}),
			"library": r(func(s *state2, sh obj, _ obj) any { return library(s, sh["library"].(int)) }),
		},
		"Stats": {
			"best": r(func(s *state2, st obj, _ obj) any { return book(s, st["best"].(string)) }),
		},
		"Viewer": {
			"favorites": r(func(s *state2, v obj, _ obj) any { return books(s, v["favorites"].([]string)) }),
			"home":      r(func(s *state2, v obj, _ obj) any { return library(s, v["home"].(int)) }),
		},
	}
}

// ---------------------------------------------------------------- queries

type diff2Query struct {
	ops   []string
	query string
	vars  func(r rnd, s *state2) map[string]any
}

func pickAny(r rnd, list ...any) any {
	v, _ := pickOf(r, list)
	return v
}

var diff2Queries = []diff2Query{
	{
		// Int ids, chains, value objects in between
		query: `{ libraries { id name stats { count best { id title author { id name } } }
      shelves { id name books { id name author { id name mentor { id name } }
      edition { year publisher { id name } } } } } }`,
	},
	{
		// nested lists
		query: `{ grid { id name sequel { id name } } libraries { id shelves { id grid { id title(upper: true) } } } }`,
	},
	{
		// unions, union members without resolvers, nullable list with nulls
		query: `{ featured { __typename ... on Book { id name author { name } } ... on Magazine { mid: id issue cover { id name } } }
      search(term: "x") { ... on Book { id title } ... on Magazine { mid: id name cover { id title } } } }`,
	},
	{
		// fragments on interfaces
		query: `fragment N on Named { name }
      fragment NodeF on Node { id ... on Author { mentor { id ...N } } ... on Book { author { id ...N } } }
      { named { __typename ...N ... on Node { ...NodeF } ... on Magazine { mid: id issue } ... on Shelf { id books { ...NodeF ...N } } } }`,
	},
	{
		// same field, different aliases and args
		query: `query Q5($u: Boolean, $l: Int) { libraries { id a: shelves { id books { id t1: title t2: title(upper: true) t3: title(upper: $u) } }
      b: shelves { id name } } x: featured { ... on Book { id r1: related(limit: 1) { id name } r2: related(limit: $l) { id title } related { id } } } }`,
		vars: func(r rnd, _ *state2) map[string]any {
			l := pickAny(r, 0, 1, 2, nil, -1)
			u := pickAny(r, true, false, nil)
			return map[string]any{"l": l, "u": u}
		},
	},
	{
		// @include / @skip
		query: `fragment L on Library { name stats { count best { id ... on Book @skip(if: $a) { title } } } }
      query Q6($a: Boolean!, $b: Boolean!) { viewer { favorites { id name @include(if: $a) author @skip(if: $b) { id name } }
      home { id ...L @include(if: $b) name shelves @include(if: $b) { id books { id name } } } } }`,
		vars: func(r rnd, _ *state2) map[string]any {
			a := r() < 0.5
			b := r() < 0.5
			return map[string]any{"a": a, "b": b}
		},
	},
	{
		// multiple operations
		ops: []string{"A", "B", "C"},
		query: `query A { libraries { id topAuthor { id name books { id name } } } }
      query B { viewer { home { id stats { best { id sequel { id sequel { id name } } } } } } }
      query C($id: Int!) { library(id: $id) { id name shelves { id library { id name } } } }`,
		vars: func(r rnd, _ *state2) map[string]any {
			return map[string]any{"id": pickAny(r, 0, 1, 2, -5, 99)}
		},
	},
	{
		// resolver root fields queried directly
		query: `query Q8($ids: [ID!]!, $nid: ID!) { books(ids: $ids) { id name author { id name } }
      node(id: $nid) { id ... on Author { name books(limit: 2) { id title } } ... on Book { name } }
      nodes(ids: $ids) { id ... on Book { sequel { id name } } } book(id: $nid) { id name } }`,
		vars: func(r rnd, s *state2) map[string]any {
			ids := []any{}
			for _, id := range filterRand(r, s.books.keys(), 0.2) {
				ids = append(ids, id)
			}
			if r() < 0.2 {
				ids = append(ids, "missing")
			}
			if r() < 0.3 {
				ids = append(ids, "b1", "b1")
			}
			candidates := append(append(append([]string{}, diff2BookIDs...), diff2AuthorIDs...), "zz")
			nid, _ := pickOf(r, candidates)
			return map[string]any{"ids": ids, "nid": nid}
		},
	},
	{
		// deep chains
		query: `{ libraries { id shelves { id books { id sequel { id author { id mentor { id mentor { id name } } } } } } } }`,
	},
	{
		// the same entities at several places
		query: `{ a: libraries { id name } b: libraries { id shelves { id library { id name shelves { id name } } } }
      viewer { home { id name } } }`,
	},
	{
		// author entity in many forms, args on entity list fields
		query: `query Q11($l: Int) { libraries { id topAuthor { id name x: books(limit: $l) { id name } y: books { id author { id name } } } } }`,
		vars: func(r rnd, _ *state2) map[string]any {
			return map[string]any{"l": pickAny(r, 0, 1, 3, nil)}
		},
	},
	{
		// shelves via named, nested grid and edition publisher chain
		query: `{ named { ... on Shelf { id name grid { id edition { year publisher { id name } } } library { id stats { count } } } } }`,
	},
	{
		// Without ids in some selections (entities selected without id by the client)
		query: `{ libraries { name shelves { name books { name author { name } } } } featured { ... on Book { title } } }`,
	},
}

// ---------------------------------------------------------------- mutations

// bookReferrers returns all entities / Query whose stored fields mention
// the given book.
func bookReferrers(s *state2, id string) []inv {
	var out []inv
	for _, b := range s.books.keys() {
		v, _ := s.books.get(b)
		if v.sequel == id || containsString(v.related, id) {
			out = append(out, inv{"Book", b})
		}
	}
	for _, sh := range s.shelves.keys() {
		v, _ := s.shelves.get(sh)
		inGrid := false
		for _, row := range v.grid {
			if containsString(row, id) {
				inGrid = true
				break
			}
		}
		if containsString(v.books, id) || inGrid {
			out = append(out, inv{"Shelf", sh})
		}
	}
	for _, l := range s.libraries.keys() {
		v, _ := s.libraries.get(l)
		if v.best == id {
			n, _ := strconv.Atoi(l)
			out = append(out, inv{"Library", n})
		}
	}
	for _, m := range s.magazines.keys() {
		v, _ := s.magazines.get(m)
		if v.cover == id {
			n, _ := strconv.Atoi(m)
			out = append(out, inv{"Magazine", n})
		}
	}
	if b, ok := s.books.get(id); ok && b.author != "" {
		out = append(out, inv{"Author", b.author}) // Author.books
	}
	out = append(out, inv{Typename: "Query"}) // root lists may mention it; be generous
	return out
}

func authorReferrers(s *state2, id string) []inv {
	var out []inv
	for _, b := range s.books.keys() {
		if v, _ := s.books.get(b); v.author == id {
			out = append(out, inv{"Book", b})
		}
	}
	for _, a := range s.authors.keys() {
		if v, _ := s.authors.get(a); v.mentor == id {
			out = append(out, inv{"Author", a})
		}
	}
	for _, l := range s.libraries.keys() {
		if v, _ := s.libraries.get(l); v.topAuthor == id {
			n, _ := strconv.Atoi(l)
			out = append(out, inv{"Library", n})
		}
	}
	return append(out, inv{Typename: "Query"})
}

func libraryReferrers(s *state2, id int) []inv {
	out := []inv{{Typename: "Query"}}
	for _, sh := range s.shelves.keys() {
		if v, _ := s.shelves.get(sh); v.library == id {
			out = append(out, inv{"Shelf", sh})
		}
	}
	return out
}

// asInt converts an Int argument (int64, or the int given as a variable).
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	panic(fmt.Sprintf("not an Int: %#v", v))
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// A mutation returns false where the TS returns null.
type diff2Mutation struct {
	name   string
	mutate func(f *fuzz2, s *state2, r rnd) ([]inv, bool)
	weight int
}

func someBook(s *state2, r rnd) (string, bool) { return pickOf(r, s.books.keys()) }

func someAuthor(s *state2, r rnd) (string, bool) { return pickOf(r, s.authors.keys()) }

func someLibrary(s *state2, r rnd) (int, bool) {
	id, ok := pickOf(r, s.libraries.keys())
	if !ok {
		return 0, false
	}
	n, _ := strconv.Atoi(id)
	return n, true
}

func anyBookOrNull(r rnd) string {
	if r() < 0.2 {
		return ""
	}
	v, _ := pickOf(r, append(append([]string{}, diff2BookIDs...), "b:new.x"))
	return v
}

func (s *state2) library(id int) *library2 {
	l, _ := s.libraries.get(strconv.Itoa(id))
	return l
}

func (s *state2) book(id string) *book2 {
	b, _ := s.books.get(id)
	return b
}

func (s *state2) shelf(id string) *shelf2 {
	sh, _ := s.shelves.get(id)
	return sh
}

var diff2Mutations = []diff2Mutation{
	{"rename book", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someBook(s, r)
		if !ok {
			return nil, false
		}
		s.book(id).name = f.word("book")
		return []inv{{"Book", id}}, true
	}, 3},
	{"retitle book", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someBook(s, r)
		if !ok {
			return nil, false
		}
		s.book(id).title = f.word("title")
		return []inv{{"Book", id}}, true
	}, 2},
	{"rename author", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someAuthor(s, r)
		if !ok {
			return nil, false
		}
		a, _ := s.authors.get(id)
		a.name = f.word("auth")
		return []inv{{"Author", id}}, true
	}, 3},
	{"rename publisher", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, _ := pickOf(r, diff2PublisherIDs)
		s.publishers.set(id, f.word("pub"))
		return []inv{{"Publisher", id}}, true
	}, 2},
	{"rename shelf", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, _ := pickOf(r, diff2ShelfIDs)
		s.shelf(id).name = f.word("shelf")
		return []inv{{"Shelf", id}}, true
	}, 2},
	{"rename library", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someLibrary(s, r)
		if !ok {
			return nil, false
		}
		s.library(id).name = f.word("lib")
		return []inv{{"Library", id}}, true
	}, 2},
	{"library count", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someLibrary(s, r)
		if !ok {
			return nil, false
		}
		s.library(id).count = f.next()
		return []inv{{"Library", id}}, true
	}, 1},
	{"library best", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someLibrary(s, r)
		if !ok {
			return nil, false
		}
		s.library(id).best = anyBookOrNull(r)
		return []inv{{"Library", id}}, true
	}, 2},
	{"library topAuthor", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someLibrary(s, r)
		if !ok {
			return nil, false
		}
		if r() < 0.2 {
			s.library(id).topAuthor = ""
		} else {
			s.library(id).topAuthor, _ = pickOf(r, diff2AuthorIDs)
		}
		return []inv{{"Library", id}}, true
	}, 2},
	{"library shelves", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someLibrary(s, r)
		if !ok {
			return nil, false
		}
		shelves := filterRand(r, diff2ShelfIDs, 0.4)
		jsRandomSort(shelves, r)
		s.library(id).shelves = shelves
		return []inv{{"Library", id}}, true
	}, 2},
	{"magazine", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, _ := pickOf(r, diff2MagazineIDs)
		m, _ := s.magazines.get(strconv.Itoa(id))
		if r() < 0.5 {
			m.issue = f.next()
		} else {
			m.cover = anyBookOrNull(r)
		}
		return []inv{{"Magazine", id}}, true
	}, 2},
	{"book author", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someBook(s, r)
		if !ok {
			return nil, false
		}
		from := s.book(id).author
		to := ""
		if r() >= 0.2 {
			to, _ = pickOf(r, diff2AuthorIDs)
		}
		s.book(id).author = to
		out := []inv{{"Book", id}}
		if from != "" {
			out = append(out, inv{"Author", from})
		}
		if to != "" {
			out = append(out, inv{"Author", to})
		}
		return out, true
	}, 3},
	{"book sequel", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someBook(s, r)
		if !ok {
			return nil, false
		}
		s.book(id).sequel = anyBookOrNull(r)
		return []inv{{"Book", id}}, true
	}, 2},
	{"book edition", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someBook(s, r)
		if !ok {
			return nil, false
		}
		b := s.book(id)
		e := b.edition
		x := r()
		switch {
		case x < 0.25:
			b.edition = nil
		case x < 0.5 || e == nil:
			publisher := ""
			if r() >= 0.2 {
				publisher, _ = pickOf(r, diff2PublisherIDs)
			}
			b.edition = &edition2{publisher: publisher, year: f.next()}
		case x < 0.75:
			e.year = f.next()
		default:
			if r() < 0.3 {
				e.publisher = ""
			} else {
				e.publisher, _ = pickOf(r, diff2PublisherIDs)
			}
		}
		return []inv{{"Book", id}}, true
	}, 3},
	{"book related", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someBook(s, r)
		if !ok {
			return nil, false
		}
		related := filterRand(r, diff2BookIDs, 0.2)
		jsRandomSort(related, r)
		s.book(id).related = related
		return []inv{{"Book", id}}, true
	}, 2},
	{"author mentor", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someAuthor(s, r)
		if !ok {
			return nil, false
		}
		a, _ := s.authors.get(id)
		if r() < 0.3 {
			a.mentor = ""
		} else {
			a.mentor, _ = pickOf(r, diff2AuthorIDs)
		}
		return []inv{{"Author", id}}, true
	}, 2},
	{"shelf books", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, _ := pickOf(r, diff2ShelfIDs)
		sh := s.shelf(id)
		sh.books = filterRand(r, withNull(diff2BookIDs, 2), 0.25)
		jsRandomSort(sh.books, r)
		if r() < 0.3 && len(sh.books) > 0 {
			sh.books = append(sh.books, sh.books[0])
		}
		return []inv{{"Shelf", id}}, true
	}, 2},
	{"shelf grid", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, _ := pickOf(r, diff2ShelfIDs)
		rows := pickIndex(r, 4)
		grid := make([][]string, rows)
		for i := range grid {
			grid[i] = filterRand(r, withNull(diff2BookIDs, 1), 0.15)
		}
		s.shelf(id).grid = grid
		return []inv{{"Shelf", id}}, true
	}, 2},
	{"shelf library", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, _ := pickOf(r, diff2ShelfIDs)
		if r() < 0.2 {
			s.shelf(id).library = nullLib
		} else {
			s.shelf(id).library, _ = pickOf(r, diff2LibraryIDs)
		}
		return []inv{{"Shelf", id}}, true
	}, 1},
	{"root featured", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		x := r()
		switch {
		case x < 0.15:
			s.featured = nil
		case x < 0.6:
			id, _ := pickOf(r, diff2BookIDs)
			s.featured = &ref2{typename: "Book", id: id}
		default:
			n, _ := pickOf(r, diff2MagazineIDs)
			s.featured = &ref2{typename: "Magazine", n: n}
		}
		return []inv{{Typename: "Query"}}, true
	}, 2},
	{"root search", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		if r() < 0.1 {
			s.search = nil
			s.searchNull = true
			return []inv{{Typename: "Query"}}, true
		}
		var all []*ref2
		for _, b := range diff2BookIDs {
			all = append(all, &ref2{typename: "Book", id: b})
		}
		for _, m := range diff2MagazineIDs {
			all = append(all, &ref2{typename: "Magazine", n: m})
		}
		all = append(all, nil, nil)
		search := filterRand(r, all, 0.2)
		jsRandomSort(search, r)
		s.search = search
		s.searchNull = false
		return []inv{{Typename: "Query"}}, true
	}, 2},
	{"root grid", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		if r() < 0.1 {
			s.grid = nil
			s.gridNull = true
		} else {
			grid := make([][]string, pickIndex(r, 4))
			for i := range grid {
				grid[i] = filterRand(r, withNull(diff2BookIDs, 1), 0.2)
			}
			s.grid = grid
			s.gridNull = false
		}
		return []inv{{Typename: "Query"}}, true
	}, 2},
	{"root named", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		var all []*ref2
		for _, b := range diff2BookIDs {
			all = append(all, &ref2{typename: "Book", id: b})
		}
		for _, a := range diff2AuthorIDs {
			all = append(all, &ref2{typename: "Author", id: a})
		}
		for _, m := range diff2MagazineIDs {
			all = append(all, &ref2{typename: "Magazine", n: m})
		}
		for _, sh := range diff2ShelfIDs {
			all = append(all, &ref2{typename: "Shelf", id: sh})
		}
		named := filterRand(r, all, 0.15)
		jsRandomSort(named, r)
		s.named = named
		return []inv{{Typename: "Query"}}, true
	}, 2},
	{"root viewer", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		if r() < 0.5 {
			favorites := filterRand(r, withNull(diff2BookIDs, 1), 0.25)
			jsRandomSort(favorites, r)
			if r() < 0.4 {
				favorites = append(favorites, favorites[:min(2, len(favorites))]...)
			}
			s.favorites = favorites
		} else if r() < 0.2 {
			s.home = nullLib
		} else {
			s.home, _ = pickOf(r, diff2LibraryIDs)
		}
		return []inv{{Typename: "Query"}}, true
	}, 2},
	{"root libraryOrder", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		order := filterRand(r, diff2LibraryIDs, 0.8)
		jsRandomSort(order, r)
		s.libraryOrder = order
		return []inv{{Typename: "Query"}}, true
	}, 1},
	{"book order", func(_ *fuzz2, s *state2, r rnd) ([]inv, bool) {
		jsRandomSort(s.bookOrder, r)
		return []inv{{Typename: "Author"}}, true
	}, 1},
	{"delete book", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someBook(s, r)
		if !ok {
			return nil, false
		}
		out := append(bookReferrers(s, id), inv{"Book", id})
		f.deleted.books.set(id, s.book(id))
		s.books.del(id)
		return out, true
	}, 1},
	{"restore book", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := pickOf(r, f.deleted.books.keys())
		if !ok {
			return nil, false
		}
		b, _ := f.deleted.books.get(id)
		s.books.set(id, b)
		f.deleted.books.del(id)
		return append(bookReferrers(s, id), inv{"Book", id}), true
	}, 1},
	{"delete author", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := someAuthor(s, r)
		if !ok {
			return nil, false
		}
		out := append(authorReferrers(s, id), inv{"Author", id})
		a, _ := s.authors.get(id)
		f.deleted.authors.set(id, a)
		s.authors.del(id)
		return out, true
	}, 1},
	{"restore author", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := pickOf(r, f.deleted.authors.keys())
		if !ok {
			return nil, false
		}
		a, _ := f.deleted.authors.get(id)
		s.authors.set(id, a)
		f.deleted.authors.del(id)
		return append(authorReferrers(s, id), inv{"Author", id}), true
	}, 1},
	{"delete library", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		n, ok := someLibrary(s, r)
		if !ok {
			return nil, false
		}
		out := append(libraryReferrers(s, n), inv{"Library", n})
		f.deleted.libraries.set(strconv.Itoa(n), s.library(n))
		s.libraries.del(strconv.Itoa(n))
		return out, true
	}, 1},
	{"restore library", func(f *fuzz2, s *state2, r rnd) ([]inv, bool) {
		id, ok := pickOf(r, f.deleted.libraries.keys())
		if !ok {
			return nil, false
		}
		n, _ := strconv.Atoi(id)
		l, _ := f.deleted.libraries.get(id)
		s.libraries.set(id, l)
		f.deleted.libraries.del(id)
		return append(libraryReferrers(s, n), inv{"Library", n}), true
	}, 1},
	{"nothing", func(*fuzz2, *state2, rnd) ([]inv, bool) { return []inv{}, true }, 1},
}

var deleteRestore = regexp.MustCompile(`delete|restore`)

func diff2Weighted() []diff2Mutation {
	var out []diff2Mutation
	for _, m := range diff2Mutations {
		if envSet("FUZZ_NO_DELETE") && deleteRestore.MatchString(m.name) {
			continue
		}
		for i := 0; i < m.weight; i++ {
			out = append(out, m)
		}
	}
	return out
}

// widen sometimes widens an entity invalidation to the whole type (always
// allowed).
func widen(invs []inv, r rnd) []inv {
	if r() < 0.12 {
		out := make([]inv, len(invs))
		for i, e := range invs {
			if e.ID != nil && r() < 0.6 {
				out[i] = inv{Typename: e.Typename}
			} else {
				out[i] = e
			}
		}
		return out
	}
	return invs
}

// ---------------------------------------------------------------- stats

type diff2Stats struct {
	mu          sync.Mutex
	Torn        int64 `json:"torn"`
	Fallback    int64 `json:"fallback"`
	FullHit     int64 `json:"fullHit"`
	LinkQueries int64 `json:"linkQueries"`
	Miss        int64 `json:"miss"`
	Partial     int64 `json:"partial"`
	Requests    int64 `json:"requests"`
	ConcReq     int64 `json:"concReq"`

	tornExample   []string
	warnings      map[string]int
	warningOrder  []string
	examples      map[string]string
	examplesOrder []string
}

func (s *diff2Stats) report(t *testing.T) {
	warnings := make([][2]any, 0, len(s.warningOrder))
	for _, k := range s.warningOrder {
		warnings = append(warnings, [2]any{k, s.warnings[k]})
	}
	if path := os.Getenv("FUZZ_STATS"); path != "" {
		line := stringify(map[string]any{"STATS": s, "TORN_EXAMPLE": s.tornExample, "WARNINGS": warnings}) + "\n"
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Error(err)
		} else {
			_, _ = file.WriteString(line)
			_ = file.Close()
		}
	} else if envSet("FUZZ_VERBOSE") {
		t.Log(stringify(s), stringify(warnings))
	}
	if path := os.Getenv("FUZZ_EXAMPLES"); path != "" {
		var parts []string
		for _, k := range s.examplesOrder {
			parts = append(parts, "### "+k+"\n"+s.examples[k])
		}
		if err := os.WriteFile(path, []byte(strings.Join(parts, "\n\n")), 0o644); err != nil {
			t.Error(err)
		}
	}
}

var warningAt = regexp.MustCompile(`at [^\n]*`)

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n])
	}
	return s
}

func (s *diff2Stats) addWarnings(warnings []string, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, full := range warnings {
		replaced := false
		k := warningAt.ReplaceAllStringFunc(full, func(m string) string {
			// Like String.replace with a non-global regex: the first match only.
			if replaced {
				return m
			}
			replaced = true
			return ""
		})
		k = truncateRunes(k, 120)
		if strings.HasPrefix(k, "Partial execution could not") {
			s.Fallback++
		}
		if _, ok := s.warnings[k]; !ok {
			s.warningOrder = append(s.warningOrder, k)
		}
		s.warnings[k]++
		var lines []string
		for _, l := range strings.Split(full, "\n") {
			if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "{") || strings.HasPrefix(l, "}") ||
				strings.HasPrefix(l, "query") || strings.HasPrefix(l, "fragment") {
				continue
			}
			lines = append(lines, l)
		}
		sig := truncateRunes(strings.Join(lines, " | "), 300)
		if _, ok := s.examples[sig]; !ok {
			s.examplesOrder = append(s.examplesOrder, sig)
			s.examples[sig] = label + " " + full
		}
	}
}

// ---------------------------------------------------------------- runner

func TestDifferential2(t *testing.T) {
	backends := fuzzBackends()
	modes := envList("FUZZ_MODES", "seq,conc,ttl")
	var variants []int
	for _, v := range envList("FUZZ_VARIANTS", "0,1,2,3") {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("FUZZ_VARIANTS: %v", err)
		}
		variants = append(variants, n)
	}
	// Defaults are smaller than the TS ones (3 seeds, 40 steps) to keep a
	// plain go test quick; FUZZ_SEEDS=3 FUZZ_STEPS=40 runs the TS defaults.
	seeds := fuzzSeeds(t, 1)
	steps := envInt(t, "FUZZ_STEPS", 25)

	stats := &diff2Stats{warnings: map[string]int{}, examples: map[string]string{}}
	defer stats.report(t)

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			for _, mode := range modes {
				for _, variant := range variants {
					for _, seed := range seeds {
						t.Run(fmt.Sprintf("mode=%s,variant=%d,seed=%d", mode, variant, seed), func(t *testing.T) {
							runDiff2Seed(t, b, mode, variant, seed, steps, stats)
						})
					}
				}
			}
		})
	}
}

type diff2Trace struct {
	step      int
	t         int64
	ext       string
	executed  []string
	expected  string
	received  string
	opName    string
	q         string
	variables string
}

type pickedQuery struct {
	q             *diff2Query
	operationName string
	variables     map[string]any
}

func runDiff2Seed(t *testing.T, b backend, mode string, variant, seed, steps int, stats *diff2Stats) {
	modeOffset := 0
	switch mode {
	case "conc":
		modeOffset = 1
	case "ttl":
		modeOffset = 2
	case "seq":
	default:
		t.Fatalf("unknown mode %q", mode)
	}
	rand := newMulberry32(seed*7919 + variant*31 + modeOffset).next

	f := &fuzz2{counter: 1000}
	f.deleted.authors = newOmap[*author2]()
	f.deleted.books = newOmap[*book2]()
	f.deleted.libraries = newOmap[*library2]()
	f.s = diff2InitialState(f, rand)

	var clk *clock
	var now func() time.Time
	ttl := 60 * time.Second
	entityTTLs := map[string]time.Duration{}
	if mode == "ttl" {
		clk = newClock()
		now = clk.Now
		ttl = 5 * time.Second
		entityTTLs = map[string]time.Duration{"Author": diff2TTLEntity, "Magazine": diff2TTLEntity * 3}
	}
	h := newHarness(t, harnessOptions{
		cache:      b.newCache(t, now),
		entityTTLs: entityTTLs,
		resolvers:  diff2Resolvers(f),
		ttl:        ttl,
		typeDefs:   diff2TypeDefs(variant),
		now:        now,
	})
	// Executions through Mangrove read the live state under a read lock,
	// mutations write it under the write lock.
	var stateMu sync.RWMutex
	useFuzzExecutor(h, &stateMu)

	var historyMu sync.Mutex
	var history []string
	addHistory := func(line string) {
		historyMu.Lock()
		history = append(history, line)
		historyMu.Unlock()
	}
	var trace []diff2Trace

	for _, q := range diff2Queries {
		if _, err := h.schema.Parse(q.query); err != nil {
			t.Fatalf("invalid test query %s: %v", q.query, err)
		}
	}

	expectedFor := func(snapshot *state2, q *diff2Query, operationName string, variables map[string]any) string {
		ctx := context.WithValue(context.WithValue(context.Background(), directKey{}, true), stateKey2{}, snapshot)
		response, err := h.schema.Execute(ctx, h.schema.MustParse(q.query), operationName, variables)
		if err != nil {
			return "execution failed: " + err.Error()
		}
		return responseJSON(response.Data, response.Errors)
	}

	failure := func(msg []string) string {
		if path := os.Getenv("FUZZ_TRACE"); path != "" && len(trace) > 0 {
			last := trace[len(trace)-1]
			var parts []string
			for _, tr := range trace {
				if tr.q != last.q || tr.opName != last.opName || tr.variables != last.variables {
					continue
				}
				parts = append(parts, fmt.Sprintf("== step %d t=%d %s\nexpected %s\nreceived %s\n%s",
					tr.step, tr.t, tr.ext, tr.expected, tr.received, strings.Join(tr.executed, "\n---\n")))
			}
			historyMu.Lock()
			content := strings.Join(parts, "\n") + "\n\nHISTORY\n" + strings.Join(history, "\n") + "\n\nSTATE\n" + f.s.dump()
			historyMu.Unlock()
			_ = os.WriteFile(path, []byte(content), 0o644)
		}
		historyMu.Lock()
		defer historyMu.Unlock()
		recent := history[max(0, len(history)-30):]
		return strings.Join(append(append(msg, "history (last 30):"), recent...), "\n")
	}

	pickQuery := func() pickedQuery {
		q, _ := pickOf(rand, diff2Queries)
		p := pickedQuery{q: &q}
		if q.ops != nil {
			p.operationName, _ = pickOf(rand, q.ops)
		}
		if q.vars != nil {
			p.variables = q.vars(rand, f.s)
		}
		if q.ops != nil && p.operationName != "C" {
			p.variables = nil
		}
		return p
	}

	nowMillis := func() int64 {
		if clk != nil {
			return clk.Now().UnixMilli()
		}
		return time.Now().UnixMilli()
	}

	runSequential := func(step int) {
		for i := 0; i < 6; i++ {
			p := pickQuery()
			expected := expectedFor(f.s, p.q, p.operationName, p.variables)
			result, err := h.tryRun(p.q.query, runOptions{operationName: p.operationName, variables: p.variables})
			if err != nil {
				t.Fatal(failure([]string{fmt.Sprintf("Run failed (variant %d, mode %s, seed %d, step %d): %v", variant, mode, seed, step, err)}))
			}
			c := result.extensions()
			partialQuery, _ := c["partialQuery"].(string)
			var pq any
			if partialQuery != "" {
				pq = len([]rune(partialQuery))
			}
			linkQueries, _ := c["linkQueries"].([]string)
			received := result.json()
			trace = append(trace, diff2Trace{
				step:      step,
				t:         nowMillis(),
				ext:       stringify(map[string]any{"hit": c["hit"], "partial": c["partial"], "pq": pq}),
				executed:  result.executed,
				expected:  expected,
				received:  received,
				opName:    p.operationName,
				q:         p.q.query,
				variables: stringify(p.variables),
			})
			stats.mu.Lock()
			stats.Requests++
			if !result.hit() {
				stats.Miss++
			} else if len(result.executed) == 0 {
				stats.FullHit++
			} else {
				stats.Partial++
			}
			stats.LinkQueries += int64(len(linkQueries))
			stats.mu.Unlock()
			if received != expected {
				t.Fatal(failure(append([]string{
					fmt.Sprintf("Mismatch (variant %d, mode %s, seed %d, step %d)", variant, mode, seed, step),
					"query: " + p.q.query,
					fmt.Sprintf("operationName: %s variables: %s", p.operationName, stringify(p.variables)),
					"expected " + expected,
					"received " + received,
					"executed:",
				}, result.executed...)))
			}
		}
	}

	weighted := diff2Weighted()
	applyMutation := func() (name string, invs []inv, ok bool) {
		m, _ := pickOf(rand, weighted)
		invs, ok = m.mutate(f, f.s, rand)
		if !ok {
			return "", nil, false
		}
		return m.name, widen(invs, rand), true
	}

	invalidate := func(invs []inv) error {
		return h.strategy.InvalidateEntities(context.Background(), entityRecords(invs))
	}

	// tick yields n times, n drawn now (as the TS draws it when called).
	tick := func() int { return pickIndex(rand, 4) }
	yield := func(n int) {
		for i := 0; i < n; i++ {
			runtime.Gosched()
		}
	}

	for step := 0; step < steps; step++ {
		if mode == "conc" && rand() < 0.3 {
			// Concurrent batch: mutations run sequentially with each other,
			// but concurrently with requests. As in the TS, the requests
			// draw their numbers before the mutator draws its later ones, so
			// the sequence of numbers doesn't depend on scheduling.
			var snapMu sync.Mutex
			snapshots := []*state2{f.s.clone()}
			var completed atomic.Int64
			nMut := 1 + pickIndex(rand, 2)
			firstTick := tick()
			nReq := 2 + pickIndex(rand, 4)
			type concRequest struct {
				pickedQuery
				tick int
			}
			requests := make([]concRequest, nReq)
			for i := range requests {
				requests[i].pickedQuery = pickQuery()
				requests[i].tick = tick()
			}

			var failMu sync.Mutex
			var failures []string
			addFailure := func(msg string) {
				failMu.Lock()
				failures = append(failures, msg)
				failMu.Unlock()
			}

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < nMut; i++ {
					if i == 0 {
						yield(firstTick)
					} else {
						yield(tick())
					}
					stateMu.Lock()
					name, invs, ok := applyMutation()
					if !ok {
						stateMu.Unlock()
						continue
					}
					snapMu.Lock()
					snapshots = append(snapshots, f.s.clone())
					v := len(snapshots) - 1
					snapMu.Unlock()
					stateMu.Unlock()
					addHistory(fmt.Sprintf("[conc %d] %s -> %s (v%d)", step, name, stringify(invs), v))
					yield(tick())
					if len(invs) > 0 {
						if err := invalidate(invs); err != nil {
							addFailure(err.Error())
						}
					}
					completed.Store(int64(v))
				}
			}()

			for _, req := range requests {
				wg.Add(1)
				go func() {
					defer wg.Done()
					yield(req.tick)
					lo := int(completed.Load())
					result, err := h.tryRun(req.q.query, runOptions{operationName: req.operationName, variables: req.variables})
					if err != nil {
						addFailure(fmt.Sprintf("Concurrent run failed (variant %d, seed %d, step %d): %v", variant, seed, step, err))
						return
					}
					snapMu.Lock()
					window := append([]*state2(nil), snapshots...)
					snapMu.Unlock()
					hi := len(window) - 1
					atomic.AddInt64(&stats.ConcReq, 1)
					got := result.json()
					matched := false
					for v := lo; v <= hi; v++ {
						if expectedFor(window[v], req.q, req.operationName, req.variables) == got {
							matched = true
							break
						}
					}
					if matched {
						return
					}
					anyIndex := -1
					for v, snapshot := range window {
						if expectedFor(snapshot, req.q, req.operationName, req.variables) == got {
							anyIndex = v
							break
						}
					}
					if lo < hi && anyIndex == -1 {
						// Torn read: mixes states while a change was in flight.
						stats.mu.Lock()
						stats.Torn++
						if len(stats.tornExample) == 0 {
							stats.tornExample = append(stats.tornExample,
								fmt.Sprintf("[variant %d seed %d step %d] %s\nreceived %s", variant, seed, step, req.q.query, got))
						}
						stats.mu.Unlock()
						return
					}
					msg := []string{
						fmt.Sprintf("Concurrent mismatch (variant %d, seed %d, step %d) window v%d..v%d; matches snapshot %d", variant, seed, step, lo, hi, anyIndex),
						"query: " + req.q.query,
						fmt.Sprintf("operationName: %s variables: %s", req.operationName, stringify(req.variables)),
					}
					for v := lo; v <= hi; v++ {
						msg = append(msg, fmt.Sprintf("expected(v%d) %s", v, expectedFor(window[v], req.q, req.operationName, req.variables)))
					}
					msg = append(msg, "received "+got)
					addFailure(strings.Join(msg, "\n"))
				}()
			}
			wg.Wait()
			if len(failures) > 0 {
				t.Fatal(failure([]string{failures[0]}))
			}
			// After everything settled, the cache must be consistent again.
			runSequential(step)
			continue
		}

		if mode == "ttl" && rand() < 0.25 {
			// Silent change to a TTL'd entity (no invalidation), then wait
			// out the TTL.
			if id, ok := pickOf(rand, f.s.authors.keys()); ok {
				a, _ := f.s.authors.get(id)
				a.name = f.word("silent")
				addHistory(fmt.Sprintf("silent rename author %s; advance %d", stringify(id), diff2TTLEntity.Milliseconds()+1))
				clk.Advance(diff2TTLEntity + time.Millisecond)
				// Magazine has 3x TTL: only change it when also waiting 3x.
				if rand() < 0.3 {
					n, _ := pickOf(rand, diff2MagazineIDs)
					m, _ := f.s.magazines.get(strconv.Itoa(n))
					m.issue = f.next()
					addHistory(fmt.Sprintf("silent magazine issue %d; advance %d", n, (diff2TTLEntity * 3).Milliseconds()))
					clk.Advance(diff2TTLEntity * 3)
				}
			}
		} else {
			name, invs, ok := applyMutation()
			if !ok {
				continue
			}
			addHistory(fmt.Sprintf("[%d] %s -> %s", step, name, stringify(invs)))
			if len(invs) > 0 {
				if err := invalidate(invs); err != nil {
					t.Fatal(err)
				}
			}
		}
		if clk != nil {
			clk.Advance(time.Duration(pickIndex(rand, 60)) * time.Millisecond)
		}

		runSequential(step)
	}

	stats.addWarnings(h.logs.Warnings(), fmt.Sprintf("[variant %d mode %s seed %d]", variant, mode, seed))
	h.expectNoErrors()
}
