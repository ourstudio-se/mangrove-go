package mangrove

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
)

func TestCacheResolverAliases(t *testing.T) {
	name := regexp.MustCompile(`^[_A-Za-z][_0-9A-Za-z]*$`)
	for _, coordinates := range []string{
		"Query.a",
		"Query.dashboard.my_field",
		"Query.a_b.c",
		"Query.a.b_c",
		"Query._private.__x",
		"Query.address_2",
		"Query.x_i3",
	} {
		for _, index := range []int{0, 7, 123} {
			alias := getCacheResolverAlias(coordinates, index)
			if !name.MatchString(alias) {
				t.Fatalf("invalid alias %q", alias)
			}
			parsed, err := parseCacheResolverAlias(alias)
			if err != nil || parsed != coordinates {
				t.Fatalf("%q round-tripped to %q (%v)", coordinates, parsed, err)
			}
		}
	}

	if getCacheResolverAlias("Query.a_b.c", 0) == getCacheResolverAlias("Query.a.b_c", 0) {
		t.Fatal("distinct coordinates share an alias")
	}

	if _, err := parseCacheResolverAlias("_ENTITY_a_x"); err == nil {
		t.Fatal("expected an error for a malformed alias")
	}
}

func TestIDLiterals(t *testing.T) {
	value, err := idToValue(CacheResolver{IDArg: "id", RootField: "x", Type: KeyTypeString}, `x"y\n`)
	if err != nil || value.Kind != ast.StringValue || value.Raw != `x"y\n` {
		t.Fatalf("unexpected %v %v", value, err)
	}

	intResolver := CacheResolver{IDArg: "id", RootField: "x", Type: KeyTypeInt}
	value, err = idToValue(intResolver, "-12")
	if err != nil || value.Kind != ast.IntValue || value.Raw != "-12" {
		t.Fatalf("unexpected %v %v", value, err)
	}
	for _, invalid := range []string{"abc", "1e3", "01", ""} {
		if _, err := idToValue(intResolver, invalid); err == nil {
			t.Fatalf("expected %q to be rejected", invalid)
		}
	}

	floatResolver := CacheResolver{IDArg: "id", RootField: "x", Type: KeyTypeFloat}
	for _, valid := range []string{"1", "-1.5", "1e3", "2.5E-3"} {
		if _, err := idToValue(floatResolver, valid); err != nil {
			t.Fatalf("expected %q to be accepted: %v", valid, err)
		}
	}
	if _, err := idToValue(floatResolver, "NaN"); err == nil {
		t.Fatal("expected NaN to be rejected")
	}
}

func mustObject(t *testing.T, s string) *Object {
	t.Helper()
	value, err := DecodeJSON([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return value.(*Object)
}

func expectSameJSON(t *testing.T, got any, want string) {
	t.Helper()
	encoded, err := EncodeJSON(got)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal(encoded, &a)
	_ = json.Unmarshal([]byte(want), &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("got %s, want %s", encoded, want)
	}
}

func TestMergeData(t *testing.T) {
	entity := func(typename, id string, rest string) string {
		if rest != "" {
			rest = "," + rest
		}
		return `{"__entityCacheId":"` + id + `","__entityCacheTypeName":"` + typename + `"` + rest + `}`
	}

	t.Run("replaces an entity with a different one at the same position", func(t *testing.T) {
		cached := mustObject(t, `{"a":`+entity("T", "1", `"x":1,"y":1`)+`}`)
		context := newMergeContext(cached, nil, nil)
		merged, err := mergeData(cloneObject(cached), mustObject(t, `{"a":`+entity("T", "2", `"x":2`)+`}`), "Query", context, true)
		if err != nil {
			t.Fatal(err)
		}
		expectSameJSON(t, merged, `{"a":`+entity("T", "2", `"x":2`)+`}`)
	})

	t.Run("fills a link stub from a snapshot at the same coordinates", func(t *testing.T) {
		cached := mustObject(t, `{"users":[{"fav":`+entity("I", "1", `"n":"one"`)+`},{"fav":`+entity("I", "2", `"n":"two"`)+`}]}`)
		context := newMergeContext(cached, nil, nil)
		merged, err := mergeData(cloneObject(cached), mustObject(t, `{"users":[{"fav":`+entity("I", "2", "")+`},{"fav":`+entity("I", "1", "")+`}]}`), "Query", context, true)
		if err != nil {
			t.Fatal(err)
		}
		expectSameJSON(t, merged, `{"users":[{"fav":`+entity("I", "2", `"n":"two"`)+`},{"fav":`+entity("I", "1", `"n":"one"`)+`}]}`)
	})

	t.Run("an identity change at a pruned position is an error", func(t *testing.T) {
		cached := mustObject(t, `{"a":`+entity("T", "1", `"b":`+entity("U", "1", `"x":1`))+`}`)
		context := newMergeContext(cached, []string{"Query.a.b"}, nil)
		_, err := mergeData(cloneObject(cached), mustObject(t, `{"a":`+entity("T", "1", `"b":`+entity("U", "2", `"x":2`))+`}`), "Query", context, true)
		var mergeError *CacheMergeError
		if !errors.As(err, &mergeError) {
			t.Fatalf("expected a CacheMergeError, got %v", err)
		}
	})

	t.Run("keeps the order of fields of the source for new objects", func(t *testing.T) {
		context := newMergeContext(nil, nil, nil)
		merged, err := mergeData(nil, mustObject(t, `{"b":1,"a":2,"c":{"z":1,"y":2}}`), "Query", context, true)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := EncodeJSON(merged)
		if string(encoded) != `{"b":1,"a":2,"c":{"z":1,"y":2}}` {
			t.Fatalf("got %s", encoded)
		}
	})
}

func TestJSONRoundTrip(t *testing.T) {
	for _, s := range []string{
		`{}`, `[]`, `null`, `true`, `false`, `0`, `-1.5e10`, `"a\"b\\c\u0001\n💥"`,
		`{"b":1,"a":[1,{"z":null,"y":"x"}],"":{}}`,
	} {
		value, err := DecodeJSON([]byte(s))
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		encoded, err := EncodeJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		var a, b any
		_ = json.Unmarshal([]byte(s), &a)
		_ = json.Unmarshal(encoded, &b)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s round-tripped to %s", s, encoded)
		}
	}
	for _, invalid := range []string{``, `{`, `[1,]`, `{"a"}`, `01x`, `"\u0001`, `{"a":1}x`} {
		if _, err := DecodeJSON([]byte(invalid)); err == nil {
			t.Fatalf("expected %q to be rejected", invalid)
		}
	}
	// Key order is kept.
	value, _ := DecodeJSON([]byte(`{"b":1,"a":2}`))
	encoded, _ := EncodeJSON(value)
	if string(encoded) != `{"b":1,"a":2}` {
		t.Fatalf("got %s", encoded)
	}
}

func TestDataPaths(t *testing.T) {
	one, zero := "1", 0
	weird := `a."b\c#@~`
	for _, path := range [][]PathPart{
		{{Field: "Query"}},
		{{Field: "Query"}, {Field: "items", Index: &zero, Typename: "Item", ID: &one}, {Field: "owner"}},
		{{Field: "Query"}, {Field: "x", ID: &weird}},
	} {
		parsed, err := StrToDataPath(DataPathToStr(path))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(parsed, path) {
			t.Fatalf("%s round-tripped to %v", DataPathToStr(path), parsed)
		}
	}
}
