package mangrove

import (
	"regexp"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// Ported from src/getPartialRecacheQuery.test.ts.

func parseDocumentForTest(t *testing.T, query string) *ast.QueryDocument {
	t.Helper()
	document, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, query)
	}
	return document
}

// testEntity mirrors the TS `_Entity` mock: numeric TS ids are written as
// their decimal string.
func testEntity(t *testing.T, typename, id string, invalidated bool, path string) entityCacheResult {
	t.Helper()
	parsed, err := StrToDataPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return entityCacheResult{
		EntityWithLocation: EntityWithLocation{Entity: Entity(typename, id), Path: parsed},
		invalidated:        invalidated,
	}
}

// printSelectionSet prints a selection set the way graphql-js prints a
// SelectionSet node: `{ ... }`.
func printSelectionSet(selectionSet ast.SelectionSet) string {
	return PrintDocument(&ast.QueryDocument{Operations: ast.OperationList{{
		Operation:    ast.Query,
		SelectionSet: selectionSet,
	}}})
}

var (
	ignoredWhitespace  = regexp.MustCompile(`[\s,]+`)
	spaceAroundPunct   = regexp.MustCompile(` ?([{}():\[\]$@=!]) ?`)
	spaceAfterEllipsis = regexp.MustCompile(`\.\.\. `)
)

// stripIgnored approximates graphql-js stripIgnoredCharacters, applied to both
// sides of a comparison.
func stripIgnored(s string) string {
	s = strings.TrimSpace(ignoredWhitespace.ReplaceAllString(s, " "))
	s = spaceAroundPunct.ReplaceAllString(s, "$1")
	return spaceAfterEllipsis.ReplaceAllString(s, "...")
}

func partialForTest(t *testing.T, document *ast.QueryDocument, entities []entityCacheResult, resolvers CacheResolverMap) *partialRecacheQuery {
	t.Helper()
	tree, err := buildEntityTree(entities, resolvers, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := getPartialRecacheQuery(document, tree, "")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func expectPrinted(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("unexpected document\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestGetPartialRecacheQuery(t *testing.T) {
	t.Run("generates simple partial query", func(t *testing.T) {
		document := parseDocumentForTest(t, `
      query FullQuery {
        dashboard {
          topActivity {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            title
            description
          }
          latestUpdates {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            date
            time
            text
          }
          todoList {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            label
            text
          }
        }
      }
    `)

		result := partialForTest(t, document, []entityCacheResult{
			testEntity(t, "Dashboard", "1", false, "Query.dashboard"),
			testEntity(t, "Activity", "2", true, "Query.dashboard.topActivity"),
			testEntity(t, "UpdateInfo", "1", false, "Query.dashboard.latestUpdates"),
			testEntity(t, "TodoItem", "1", false, "Query.dashboard.todoList"),
		}, CacheResolverMap{})

		if result == nil {
			t.Fatal("Partial document not created")
		}

		expectPrinted(t, PrintDocument(result.query), `query FullQuery {
  dashboard {
    topActivity {
      __entityCacheId: id
      __entityCacheTypeName: __typename
      title
      description
    }
  }
}`)
	})

	t.Run("always select full selection sets when traversing nonentity object types", func(t *testing.T) {
		document := parseDocumentForTest(t, `
        query FullQuery {
          dashboard {
            someOtherField {
              someOtherStuff
            }
            activities {
              id
              author {
                __entityCacheId: id
                __entityCacheTypeName: __typename
                name
              }
              category {
                name
              }
            }
          }
        }
      `)

		result := partialForTest(t, document, []entityCacheResult{
			testEntity(t, "Dashboard", "1", false, "Query.dashboard"),
			testEntity(t, "User", "2", true, "Query.dashboard.activities.author"),
		}, CacheResolverMap{})

		if result == nil {
			t.Fatal("Partial document not created")
		}

		expectPrinted(t, PrintDocument(result.query), `query FullQuery {
  dashboard {
    activities {
      id
      author {
        __entityCacheId: id
        __entityCacheTypeName: __typename
        name
      }
      category {
        name
      }
    }
  }
}`)
	})

	t.Run("always take full selection when traversing a list", func(t *testing.T) {
		document := parseDocumentForTest(t, `
      query FullQuery {
        dashboard {
          activities {
            author {
              name
            }
            thisShouldBeIncluded
          }
          thisShouldBePruned
        }
      }
    `)

		result := partialForTest(t, document, []entityCacheResult{
			testEntity(t, "Dashboard", "1", false, "Query.dashboard"),
			testEntity(t, "Activity", "1", false, "Query.dashboard.activities@0#1"),
			testEntity(t, "User", "1", true, "Query.dashboard.activities@0#1.author"),
		}, CacheResolverMap{})

		if result == nil {
			t.Fatal("No result")
		}

		expectPrinted(t, PrintDocument(result.query), `query FullQuery {
  dashboard {
    activities {
      author {
        name
      }
      thisShouldBeIncluded
    }
  }
}`)
	})

	t.Run("returns null for query without invalidations", func(t *testing.T) {
		document := parseDocumentForTest(t, `
      query FullQuery {
        dashboard {
          topActivity {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            title
            description
          }
          latestUpdates {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            date
            time
            text
          }
          todoList {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            label
            text
          }
        }
      }
    `)

		result := partialForTest(t, document, []entityCacheResult{}, CacheResolverMap{})

		if result != nil {
			t.Fatalf("expected nil, got:\n%s", PrintDocument(result.query))
		}
	})

	t.Run("correctly filters out dead branches", func(t *testing.T) {
		document := parseDocumentForTest(t, `
        query FullQuery {
          dashboard {
            topActivity {
              __entityCacheId: id
              __entityCacheTypeName: __typename
              title
              description
            }
            latestUpdates {
              __entityCacheId: id
              __entityCacheTypeName: __typename
              date
              time
              text
            }
            todoList {
              __entityCacheId: id
              __entityCacheTypeName: __typename
              label
              text
            }
          }
          someCachedData {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            name
          }
        }
      `)

		result := partialForTest(t, document, []entityCacheResult{
			testEntity(t, "Dashboard", "1", false, "Query.dashboard"),
			testEntity(t, "Activity", "2", true, "Query.dashboard.topActivity"),
			testEntity(t, "Activity", "3", false, "Query.someCachedData"),
		}, CacheResolverMap{})

		if result == nil {
			t.Fatal("Partial document not created")
		}

		expectPrinted(t, PrintDocument(result.query), `query FullQuery {
  dashboard {
    topActivity {
      __entityCacheId: id
      __entityCacheTypeName: __typename
      title
      description
    }
  }
}`)
	})

	t.Run("traverses multiple definitions", func(t *testing.T) {
		document := parseDocumentForTest(t, `
        fragment Activity on Activity {
          __entityCacheId: id
          __entityCacheTypeName: __typename
          title
          description
          relatedUpdate {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            date
            ...SystemUpdateInfo
            ...UserUpdateInfo
          }
        }

        fragment SystemUpdateInfo on SystemUpdateInfo {
          version
        }

        fragment UserUpdateInfo on UserUpdateInfo {
          byUser
        }

        fragment TodoItem on TodoItem {
          __entityCacheId: id
          __entityCacheTypeName: __typename
          label
          text
        }

        query FullQuery {
          dashboard {
            topActivity {
              ...Activity
              someField
            }
            latestUpdates {
              __entityCacheId: id
              __entityCacheTypeName: __typename
              ...SystemUpdateInfo
              ...UserUpdateInfo
            }
            todoList {
              ...TodoItem
            }
          }
        }
      `)

		result := partialForTest(t, document, []entityCacheResult{
			testEntity(t, "UpdateInfo", "2", true, "Query.dashboard.topActivity.relatedUpdate"),
			testEntity(t, "Dashboard", "1", false, "Query.dashboard"),
			testEntity(t, "Activity", "1", false, "Query.dashboard.topActivity"),
		}, CacheResolverMap{})

		if result == nil {
			t.Fatal("Partial document not created")
		}

		expectPrinted(t, PrintDocument(result.query), `query FullQuery {
  dashboard {
    topActivity {
      ... on Activity {
        __entityCacheId: id
        __entityCacheTypeName: __typename
        relatedUpdate {
          __entityCacheId: id
          __entityCacheTypeName: __typename
          date
          ... on SystemUpdateInfo {
            version
          }
          ... on UserUpdateInfo {
            byUser
          }
        }
      }
    }
  }
}`)
	})

	const resolutionDocument = `
        fragment Activity on Activity {
          __entityCacheId: id
          __entityCacheTypeName: __typename
          title
          description
          relatedUpdate {
            ...UpdateInfo
          }
        }

        fragment UpdateInfo on UpdateInfo {
          __entityCacheId: id
          __entityCacheTypeName: __typename
          date
          time
          text
        }

        fragment TodoItem on TodoItem {
          __entityCacheId: id
          __entityCacheTypeName: __typename
          label
          text
        }

        query FullQuery {
          dashboard {
            user {
              __entityCacheId: id
              __entityCacheTypeName: __typename
              name
            }
            topActivity {
              ...Activity
            }
            latestUpdates {
              ...UpdateInfo
              nonFragmentField
            }
            todoList {
              ...TodoItem
            }
          }
        }
      `

	const relatedUpdateLinkSelection = `{
          ... on UpdateInfo {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            date
            time
            text
          }
        }`

	const latestUpdatesLinkSelection = `{
          ... on UpdateInfo {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            date
            time
            text
          }
          nonFragmentField
        }`

	t.Run("creates singleton entity resolution", func(t *testing.T) {
		document := parseDocumentForTest(t, resolutionDocument)

		result := partialForTest(t, document, []entityCacheResult{
			testEntity(t, "UpdateInfo", "2", true, `Query.dashboard.latestUpdates@7#"2"`),
			testEntity(t, "UpdateInfo", "2", true, "Query.dashboard.topActivity.relatedUpdate"),
			testEntity(t, "User", "123", true, "Query.dashboard.user"),
			testEntity(t, "Dashboard", "1", false, "Query.dashboard"),
		}, CacheResolverMap{
			"UpdateInfo": {IDArg: "id", RootField: "getUpdateInfo", Type: KeyTypeInt},
		})

		if result == nil {
			t.Fatal("Partial document not created")
		}

		if len(result.linkSelections) != 2 {
			t.Fatalf("expected 2 link selections, got %d", len(result.linkSelections))
		}
		if _, ok := result.linkSelections["Query.dashboard.topActivity.relatedUpdate"]; !ok {
			t.Fatal("missing link selection for Query.dashboard.topActivity.relatedUpdate")
		}

		expectPrinted(t,
			stripIgnored(printSelectionSet(result.linkSelections["Query.dashboard.topActivity.relatedUpdate"])),
			stripIgnored(relatedUpdateLinkSelection))
		expectPrinted(t,
			stripIgnored(printSelectionSet(result.linkSelections["Query.dashboard.latestUpdates"])),
			stripIgnored(latestUpdatesLinkSelection))

		expectPrinted(t, PrintDocument(result.query), `query FullQuery {
  dashboard {
    user {
      __entityCacheId: id
      __entityCacheTypeName: __typename
      name
    }
    latestUpdates {
      ... on UpdateInfo {
        __entityCacheId: id
        __entityCacheTypeName: __typename
      }
    }
  }
  _ENTITY_dashboard__topActivity__relatedUpdate_i0: getUpdateInfo(id: 2) {
    ... on UpdateInfo {
      __entityCacheId: id
      __entityCacheTypeName: __typename
      date
      time
      text
    }
  }
  _ENTITY_dashboard__latestUpdates_i1: getUpdateInfo(id: 2) {
    ... on UpdateInfo {
      __entityCacheId: id
      __entityCacheTypeName: __typename
      date
      time
      text
    }
    nonFragmentField
  }
}`)
	})

	t.Run("creates a mapped cache resolution", func(t *testing.T) {
		document := parseDocumentForTest(t, resolutionDocument)

		result := partialForTest(t, document, []entityCacheResult{
			testEntity(t, "UpdateInfo", "2", true, `Query.dashboard.latestUpdates@2#"7"`),
			testEntity(t, "UpdateInfo", "3", true, `Query.dashboard.latestUpdates@3#"4"`),
			testEntity(t, "UpdateInfo", "2", true, "Query.dashboard.topActivity.relatedUpdate"),
			testEntity(t, "User", "123", true, "Query.dashboard.user"),
			testEntity(t, "Dashboard", "1", false, "Query.dashboard"),
		}, CacheResolverMap{
			"UpdateInfo": {Batch: true, IDArg: "id", RootField: "getUpdateInfo", Type: KeyTypeInt},
		})

		if result == nil {
			t.Fatal("Partial document not created")
		}

		expectPrinted(t,
			stripIgnored(printSelectionSet(result.linkSelections["Query.dashboard.topActivity.relatedUpdate"])),
			stripIgnored(relatedUpdateLinkSelection))
		expectPrinted(t,
			stripIgnored(printSelectionSet(result.linkSelections["Query.dashboard.latestUpdates"])),
			stripIgnored(latestUpdatesLinkSelection))

		expectPrinted(t, PrintDocument(result.query), `query FullQuery {
  dashboard {
    user {
      __entityCacheId: id
      __entityCacheTypeName: __typename
      name
    }
    latestUpdates {
      ... on UpdateInfo {
        __entityCacheId: id
        __entityCacheTypeName: __typename
      }
    }
  }
  _ENTITY_dashboard__topActivity__relatedUpdate_i0: getUpdateInfo(id: [2]) {
    ... on UpdateInfo {
      __entityCacheId: id
      __entityCacheTypeName: __typename
      date
      time
      text
    }
  }
  _ENTITY_dashboard__latestUpdates_i1: getUpdateInfo(id: [2, 3]) {
    ... on UpdateInfo {
      __entityCacheId: id
      __entityCacheTypeName: __typename
      date
      time
      text
    }
    nonFragmentField
  }
}`)
	})

	t.Run("avoid conflicting cache resolution when invalidating both list query and downstream entity using a cache resolver", func(t *testing.T) {
		document := parseDocumentForTest(t, `query MyQuery {
        listQuery {
          __entityCacheId: id
          __entityCacheTypeName: __typename
          name
          items {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            name
          }
        }
      }`)

		result := partialForTest(t, document, []entityCacheResult{
			testEntity(t, "Activity", "1", true, "Query.listQuery"),
			testEntity(t, "TodoItem", "1", true, "Query.listQuery.items@0#1"),
		}, CacheResolverMap{
			"TodoItem": {IDArg: "id", RootField: "getRelatedEntity", Type: KeyTypeString},
		})

		if result == nil {
			t.Fatal("Partial document not created")
		}

		// Since a node above the invalidated RelatedEntity has been
		// invalidated, we should not be fetching it separately using a cache
		// resolver.
		expectPrinted(t, PrintDocument(result.query), `query MyQuery {
  listQuery {
    __entityCacheId: id
    __entityCacheTypeName: __typename
    name
    items {
      __entityCacheId: id
      __entityCacheTypeName: __typename
    }
  }
  _ENTITY_listQuery__items_i0: getRelatedEntity(id: "1") {
    __entityCacheId: id
    __entityCacheTypeName: __typename
    name
  }
}`)
	})

	t.Run("avoids conflicting cache resolution when invalidating both list query and downstream entity using a cache resolver, where the entity is inside a fragment", func(t *testing.T) {
		document := parseDocumentForTest(t, `fragment SomeObject on SomeObject {
        anotherObject {
          relatedEntity {
            __entityCacheId: id
            __entityCacheTypeName: __typename
            name
          }
        }
      }

      query MyQuery {
        listQuery {
          __entityCacheId: id
          __entityCacheTypeName: __typename
          name
          items {
            ...SomeObject
          }
        }
      }`)

		result := partialForTest(t, document, []entityCacheResult{
			testEntity(t, "Activity", "1", true, "Query.listQuery"),
			testEntity(t, "TodoItem", "2", true, "Query.listQuery.items@0.anotherObject.relatedEntity"),
		}, CacheResolverMap{
			"TodoItem": {IDArg: "id", RootField: "getRelatedEntity", Type: KeyTypeString},
		})

		if result == nil {
			t.Fatal("Partial document not created")
		}

		// listQuery was invalidated, so everything below it up to the next
		// cache resolver is refetched, including the membership of `items`
		// (which leads to the relatedEntity links).
		//
		// graphql-js wraps the arguments of the cache resolution over several
		// lines (`getRelatedEntity(\n    id: "2"\n  )`) since the line exceeds
		// 80 characters; Mangrove's printer never wraps arguments, which is
		// only a formatting difference.
		expectPrinted(t, PrintDocument(result.query), `query MyQuery {
  listQuery {
    __entityCacheId: id
    __entityCacheTypeName: __typename
    name
    items {
      ... on SomeObject {
        anotherObject {
          relatedEntity {
            __entityCacheId: id
            __entityCacheTypeName: __typename
          }
        }
      }
    }
  }
  _ENTITY_listQuery__items__anotherObject__relatedEntity_i0: getRelatedEntity(id: "2") {
    __entityCacheId: id
    __entityCacheTypeName: __typename
    name
  }
}`)
	})

	t.Run("collects link selections from cache resolver breakpoints", func(t *testing.T) {
		document := DecorateClientDocument(parseDocumentForTest(t, `
        query Query {
          rootA {
            id @idField
            b {
              id @idField
              c {
                id @idField
              }
            }
          }
        }
      `))

		result := partialForTest(t, document, []entityCacheResult{
			testEntity(t, "A", "1", true, "Query.rootA"),
			testEntity(t, "B", "2", false, "Query.rootA.b"),
			testEntity(t, "C", "3", false, "Query.rootA.b.c"),
		}, CacheResolverMap{
			"A": {IDArg: "id", RootField: "a", Type: KeyTypeString},
			"B": {IDArg: "id", RootField: "b", Type: KeyTypeString},
			"C": {IDArg: "id", RootField: "C", Type: KeyTypeString},
		})

		if result == nil {
			t.Fatal("Partial document not created")
		}

		expectPrinted(t, printSelectionSet(result.linkSelections["Query.rootA"]), `{
  id
  b {
    __entityCacheTypeName: __typename
    __entityCacheId: id
  }
  __entityCacheTypeName: __typename
  __entityCacheId: id
}`)

		expectPrinted(t, printSelectionSet(result.linkSelections["Query.rootA.b"]), `{
  id
  c {
    __entityCacheTypeName: __typename
    __entityCacheId: id
  }
  __entityCacheTypeName: __typename
  __entityCacheId: id
}`)

		expectPrinted(t, printSelectionSet(result.linkSelections["Query.rootA.b.c"]), `{
  id
  __entityCacheTypeName: __typename
  __entityCacheId: id
}`)
	})

	t.Run("complex realworld example", func(t *testing.T) {
		document := parseDocumentForTest(t, `
        fragment Address on FleetAccountCompanyAddress {
          line1
          line2
          locality
          countryCode
          postalCode
        }

        fragment Company on FleetAccountCompany {
          address {
            ...Address
          }
          contactEmailAddress
          contactPhoneNumber
          name
          organizationNumber
          taxIdentifier
          businessEmailAddress
          invoiceAddress {
            ...Address
          }
        }

        query {
          sysAllCases {
            __entityCacheId: key
            __entityCacheTypeName: __typename
            listType
            nodes {
              __entityCacheId: key
              __entityCacheTypeName: __typename
              accountConnection {
                __entityCacheId: key
                __entityCacheTypeName: __typename
                node {
                  __entityCacheId: key
                  __entityCacheTypeName: __typename
                  company {
                    ...Company
                  }
                  desiredFleetSize
                  sellableMarket
                  status
                  accountType
                  approvalStatus
                  agreement {
                    status
                    discountPlanId
                  }
                  invitesConnection {
                    __entityCacheId: key
                    __entityCacheTypeName: __typename
                    nodes {
                      __entityCacheId: key
                      __entityCacheTypeName: __typename
                      emailAddress
                      role
                      created
                    }
                  }
                  approvalRequirementsConnection {
                    __entityCacheId: key
                    __entityCacheTypeName: __typename
                    nodes {
                      __entityCacheId: key
                      __entityCacheTypeName: __typename
                      name
                      isCompleted
                    }
                  }
                  referencesConnection {
                    __entityCacheId: key
                    __entityCacheTypeName: __typename
                    nodes {
                      role
                      volvoId
                    }
                  }
                }
              }
            }
          }
        }
      `)

		result := partialForTest(t, document, []entityCacheResult{
			testEntity(t, "FleetCaseAccountConnection",
				"7df30e0d149b0b540536b05647725fc0779a38265190e4b69182dbfc8aaf12fe",
				true,
				"Query.sysAllCases.nodes@0.accountConnection"),
			testEntity(t, "SysAllCases", "1", false, "Query.sysAllCases"),
			testEntity(t, "FleetAccount", "1", false, "Query.sysAllCases.nodes@0"),
		}, CacheResolverMap{})

		if result == nil {
			t.Fatal("Partial document not created")
		}

		expectPrinted(t, PrintDocument(result.query), `{
  sysAllCases {
    __entityCacheId: key
    __entityCacheTypeName: __typename
    nodes {
      __entityCacheId: key
      __entityCacheTypeName: __typename
      accountConnection {
        __entityCacheId: key
        __entityCacheTypeName: __typename
        node {
          __entityCacheId: key
          __entityCacheTypeName: __typename
          company {
            ... on FleetAccountCompany {
              address {
                ... on FleetAccountCompanyAddress {
                  line1
                  line2
                  locality
                  countryCode
                  postalCode
                }
              }
              contactEmailAddress
              contactPhoneNumber
              name
              organizationNumber
              taxIdentifier
              businessEmailAddress
              invoiceAddress {
                ... on FleetAccountCompanyAddress {
                  line1
                  line2
                  locality
                  countryCode
                  postalCode
                }
              }
            }
          }
          desiredFleetSize
          sellableMarket
          status
          accountType
          approvalStatus
          agreement {
            status
            discountPlanId
          }
          invitesConnection {
            __entityCacheId: key
            __entityCacheTypeName: __typename
            nodes {
              __entityCacheId: key
              __entityCacheTypeName: __typename
              emailAddress
              role
              created
            }
          }
          approvalRequirementsConnection {
            __entityCacheId: key
            __entityCacheTypeName: __typename
            nodes {
              __entityCacheId: key
              __entityCacheTypeName: __typename
              name
              isCompleted
            }
          }
          referencesConnection {
            __entityCacheId: key
            __entityCacheTypeName: __typename
            nodes {
              role
              volvoId
            }
          }
        }
      }
    }
  }
}`)
	})
}
