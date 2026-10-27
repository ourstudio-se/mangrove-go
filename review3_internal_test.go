package mangrove

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// Findings of a review of the Go port.

func entries[K, V any](m *weakMemo[K, V]) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

func TestDecorationMemosDontKeepDocumentsAlive(t *testing.T) {
	schema := gqlparser.MustLoadSchema(&ast.Source{Input: `type Query { a: Int } type Mutation { b: Int }`})
	config := ConfigureFromSchema(schema, []string{"id"}, nil)
	decorator := &schemaDecorator{schema: schema}
	_ = config

	for _, query := range []string{
		`query Q { a } mutation M { b }`,
		`{ __entityCacheTypeName: __typename a }`,
	} {
		for i := 0; i < 100; i++ {
			document, err := parser.ParseQuery(&ast.Source{Input: query})
			if err != nil {
				t.Fatal(err)
			}
			decorator.decorate(document)
			DecorateClientDocument(document)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for entries(&decorator.memo)+entries(&clientDecorations) > 0 && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if n := entries(&decorator.memo) + entries(&clientDecorations); n > 0 {
		t.Fatalf("%d memo entries survived", n)
	}
}

func TestClientWrittenMarkersDontPreventDecoration(t *testing.T) {
	schema := gqlparser.MustLoadSchema(&ast.Source{Input: `type Query { items: [Item!]! } type Item { id: ID! }`})
	config := ConfigureFromSchema(schema, []string{"id"}, nil)
	document, err := parser.ParseQuery(&ast.Source{Input: `{ __entityCacheTypeName: __typename items { id } }`})
	if err != nil {
		t.Fatal(err)
	}
	if printed := PrintDocument(config.Decorate(document)); !strings.Contains(printed, "__entityCacheId: id") {
		t.Fatalf("not decorated:\n%s", printed)
	}
}

func TestJSONDecoderLimits(t *testing.T) {
	// Deep nesting is an error, not a crash.
	if _, err := DecodeJSON([]byte(strings.Repeat("[", 1_000_000))); err == nil {
		t.Fatal("expected an error")
	}
	nested := strings.Repeat("[", 100) + strings.Repeat("]", 100)
	if _, err := DecodeJSON([]byte(nested)); err != nil {
		t.Fatal(err)
	}

	// Wide objects decode in linear time.
	var wide strings.Builder
	wide.WriteString("{")
	for i := 0; i < 200_000; i++ {
		if i > 0 {
			wide.WriteString(",")
		}
		wide.WriteString(`"k`)
		wide.WriteString(strings.Repeat("x", i%7))
		wide.WriteString(time.Duration(i).String())
		wide.WriteString(`":1`)
	}
	wide.WriteString("}")
	start := time.Now()
	value, err := DecodeJSON([]byte(wide.String()))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("decoding a wide object took %s", elapsed)
	}
	object := value.(*Object)
	if v, ok := object.Get("k0s"); !ok || v == nil {
		t.Fatal("lookup in a wide object failed")
	}

	// Duplicate keys keep their first position and their last value, as
	// JSON.parse does.
	value, _ = DecodeJSON([]byte(`{"a":1,"b":2,"a":3}`))
	encoded, _ := EncodeJSON(value)
	if string(encoded) != `{"a":3,"b":2}` {
		t.Fatalf("got %s", encoded)
	}

	for _, invalid := range []string{`01`, `-01`, `[00]`} {
		if _, err := DecodeJSON([]byte(invalid)); err == nil {
			t.Fatalf("expected %s to be rejected", invalid)
		}
	}

	// Invalid UTF-8 is replaced, escaped or not, as the encoder does.
	for _, raw := range []string{"\"a\xffb\"", "\"a\xffb\\n\""} {
		value, err := DecodeJSON([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(value.(string), "�") {
			t.Fatalf("%q decoded to %q", raw, value)
		}
	}
}

func TestPrinterKeepsFragmentVariables(t *testing.T) {
	document, err := parser.ParseQuery(&ast.Source{Input: `fragment F($n: Int = 3) on Query { a(n: $n) } { ...F }`})
	if err != nil {
		t.Skip("fragment variables not supported by this parser:", err)
	}
	if printed := PrintDocument(document); !strings.Contains(printed, "fragment F($n: Int = 3) on Query") {
		t.Fatalf("got:\n%s", printed)
	}
}
