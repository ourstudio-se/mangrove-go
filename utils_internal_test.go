package mangrove

import (
	"reflect"
	"strings"
	"testing"
)

// Ported from src/utils.test.ts. memoInlineFragments is inlineFragments in
// Go.

func TestInlineFragments(t *testing.T) {
	t.Run("correctly inlines all fragments", func(t *testing.T) {
		document := parseDocumentForTest(t, `
      fragment A on A {
        b {
          ...B
        }
      }

      fragment B on B {
        c {
          ... on D {
            e
          }
        }
      }

      query Q {
        a {
          ...A
        }
        f {
          ... on F {
            g
          }
        }
      }
    `)

		result, err := inlineFragments(document)
		if err != nil {
			t.Fatal(err)
		}

		expectPrinted(t, PrintDocument(result), `query Q {
  a {
    ... on A {
      b {
        ... on B {
          c {
            ... on D {
              e
            }
          }
        }
      }
    }
  }
  f {
    ... on F {
      g
    }
  }
}`)
	})
}

func TestInlineFragmentsWithDirectives(t *testing.T) {
	t.Run("keeps directives of fragment spreads", func(t *testing.T) {
		result, err := inlineFragments(parseDocumentForTest(t, `
        fragment D on Dashboard {
          top {
            id
          }
        }
        query Q($x: Boolean!) {
          dashboard {
            ...D @include(if: $x)
          }
        }
      `))
		if err != nil {
			t.Fatal(err)
		}

		printed := PrintDocument(result)
		if !strings.Contains(printed, "... on Dashboard @include(if: $x)") {
			t.Fatalf("expected the spread's directive in:\n%s", printed)
		}
	})

	t.Run("rejects cyclic fragments", func(t *testing.T) {
		_, err := inlineFragments(parseDocumentForTest(t, `
          fragment A on T {
            ...B
          }
          fragment B on T {
            ...A
          }
          query {
            t {
              ...A
            }
          }
        `))
		// TS: toThrow(/Cyclic/); the Go message is lowercase ("cyclic
		// fragment spread: A"), so match case-insensitively.
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cyclic") {
			t.Fatalf("expected a cyclic fragment error, got %v", err)
		}
	})
}

func TestPruneUnusedVariables(t *testing.T) {
	t.Run("drops definitions of variables no longer used", func(t *testing.T) {
		document := parseDocumentForTest(t, `
      query Q($a: Int, $b: Int, $c: Boolean!) {
        x(a: $a) @include(if: $c)
      }
    `)
		pruned := pruneUnusedVariables(document.Operations[0])

		var names []string
		for _, definition := range pruned.VariableDefinitions {
			names = append(names, definition.Variable)
		}
		if !reflect.DeepEqual(names, []string{"a", "c"}) {
			t.Fatalf("got %v, want [a c]", names)
		}
	})
}

func TestDataPathNotation(t *testing.T) {
	for _, str := range []string{
		`Query.items@0#"a.b@c#d"`,
		`Query.items~Item@0#"1".owner~Owner#"5"`,
		`Query.x#"say \"hi\""`,
	} {
		t.Run("round-trips "+str, func(t *testing.T) {
			path, err := StrToDataPath(str)
			if err != nil {
				t.Fatal(err)
			}
			if got := DataPathToStr(path); got != str {
				t.Fatalf("got %s, want %s", got, str)
			}
		})
	}

	t.Run("parses typenames, indices and ids", func(t *testing.T) {
		path, err := StrToDataPath(`Query.items~Item@2#"a.b"`)
		if err != nil {
			t.Fatal(err)
		}
		index, id := 2, "a.b"
		want := []PathPart{
			{Field: "Query"},
			{Field: "items", ID: &id, Index: &index, Typename: "Item"},
		}
		if !reflect.DeepEqual(path, want) {
			t.Fatalf("got %+v, want %+v", path, want)
		}
	})
}
