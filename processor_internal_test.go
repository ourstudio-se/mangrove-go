package mangrove

import (
	"context"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

// Ported from src/resultProcessor.test.ts.

const processorCachedDashboard = `{
  "dashboard": {
    "latestUpdates": [
      {
        "__entityCacheId": "1",
        "__entityCacheTypeName": "UpdateInfo",
        "__typename": "UpdateInfo",
        "date": "2023-12-04T00:00:00Z",
        "id": "1",
        "text": "Lorem ipsum"
      },
      {
        "__entityCacheId": "2",
        "__entityCacheTypeName": "UpdateInfo",
        "__typename": "UpdateInfo",
        "date": "2023-12-03T00:00:00Z",
        "id": "2",
        "text": "Dolor hic set amit"
      }
    ],
    "topActivity": {
      "__entityCacheId": 2,
      "__entityCacheTypeName": "Activity",
      "__typename": "Activity",
      "description": "Lorem ipsum dolor",
      "id": 2,
      "title": "Lorem ipsum"
    }
  }
}`

func newTestResultProcessor(store func(context.Context, StoreParams) error, awaitWrite bool) *resultProcessor {
	return &resultProcessor{
		awaitWriteBeforeResponse: awaitWrite,
		collectEntity:            DefaultCollectEntity,
		entityTTLs:               map[string]time.Duration{},
		logger:                   slog.Default(),
		shouldCacheResult:        DefaultShouldCacheResult,
		store:                    store,
		ttl:                      time.Hour,
	}
}

func noopStore(context.Context, StoreParams) error { return nil }

// processedAt returns the value at a path of processed data.
func processedAt(t *testing.T, data *Object, steps ...any) any {
	t.Helper()
	var value any = data
	for _, step := range steps {
		switch s := step.(type) {
		case string:
			object, ok := value.(*Object)
			if !ok || object == nil {
				t.Fatalf("no field %q in %v", s, value)
			}
			value, _ = object.Get(s)
		case int:
			items, ok := value.([]any)
			if !ok || s >= len(items) {
				t.Fatalf("no index %d in %v", s, value)
			}
			value = items[s]
		}
	}
	return value
}

func TestResultProcessor(t *testing.T) {
	t.Run("collects entities", func(t *testing.T) {
		nextResult := &executionResult{data: mustObject(t, processorCachedDashboard)}

		type storedCall struct {
			cacheKey          string
			collectedEntities []EntityWithLocation
			ttl               time.Duration
		}
		var stored []storedCall

		processor := newTestResultProcessor(func(_ context.Context, params StoreParams) error {
			stored = append(stored, storedCall{params.CacheKey, params.CollectedEntities, params.TTL})
			return nil
		}, true)

		if _, err := processor.process(context.Background(), processResultParams{
			cacheKey:         "abc123",
			nextResults:      []*executionResult{nextResult},
			originalDocument: parseDocumentForTest(t, `query { a }`),
		}); err != nil {
			t.Fatal(err)
		}

		if len(stored) != 1 {
			t.Fatalf("expected 1 store, got %d", len(stored))
		}
		if stored[0].cacheKey != "abc123" {
			t.Fatalf("cache key %q", stored[0].cacheKey)
		}
		if stored[0].ttl != time.Hour {
			t.Fatalf("ttl %v", stored[0].ttl)
		}

		ref := func(s string) *string { return &s }
		idx := func(i int) *int { return &i }
		want := []EntityWithLocation{
			{
				Entity: Entity("UpdateInfo", "1"),
				Path: []PathPart{
					{Field: "Query"},
					{Field: "dashboard"},
					{Field: "latestUpdates", ID: ref("1"), Index: idx(0), Typename: "UpdateInfo"},
				},
			},
			{
				Entity: Entity("UpdateInfo", "2"),
				Path: []PathPart{
					{Field: "Query"},
					{Field: "dashboard"},
					{Field: "latestUpdates", ID: ref("2"), Index: idx(1), Typename: "UpdateInfo"},
				},
			},
			{
				Entity: Entity("Activity", "2"),
				Path: []PathPart{
					{Field: "Query"},
					{Field: "dashboard"},
					{Field: "topActivity", ID: ref("2"), Typename: "Activity"},
				},
			},
		}
		if !reflect.DeepEqual(stored[0].collectedEntities, want) {
			t.Fatalf("collected entities:\n got: %s\nwant: %s", describeEntities(stored[0].collectedEntities), describeEntities(want))
		}
	})

	t.Run("merges partial result into cached result", func(t *testing.T) {
		nextResult := &executionResult{data: mustObject(t, `{
  "dashboard": {
    "topActivity": {
      "__entityCacheId": 2,
      "__entityCacheTypeName": "Activity",
      "__typename": "Activity",
      "description": "Lorem ipsum dolor",
      "id": 2,
      "title": "[Changed] Lorem ipsum"
    }
  }
}`)}

		processed, err := newTestResultProcessor(noopStore, false).process(context.Background(), processResultParams{
			cacheKey:         "abc123",
			cachedResult:     &CachedResult{Data: mustObject(t, processorCachedDashboard)},
			nextResults:      []*executionResult{nextResult},
			originalDocument: parseDocumentForTest(t, `query { a }`),
		})
		if err != nil {
			t.Fatal(err)
		}

		expectSameJSON(t, processedAt(t, processed.data, "dashboard", "topActivity"), `{
  "__typename": "Activity",
  "description": "Lorem ipsum dolor",
  "id": 2,
  "title": "[Changed] Lorem ipsum"
}`)
		expectSameJSON(t, processedAt(t, processed.data, "dashboard", "latestUpdates", 0), `{
  "__typename": "UpdateInfo",
  "date": "2023-12-04T00:00:00Z",
  "id": "1",
  "text": "Lorem ipsum"
}`)
		expectSameJSON(t, processedAt(t, processed.data, "dashboard", "latestUpdates", 1), `{
  "__typename": "UpdateInfo",
  "date": "2023-12-03T00:00:00Z",
  "id": "2",
  "text": "Dolor hic set amit"
}`)
	})

	t.Run("maps non batch cache resolver results into main data tree", func(t *testing.T) {
		nextResult := &executionResult{data: mustObject(t, `{
  "`+getCacheResolverAlias("Query.dashboard.latestUpdates", 0)+`": {
    "__entityCacheId": "2",
    "__entityCacheTypeName": "UpdateInfo",
    "__typename": "UpdateInfo",
    "date": "2023-12-03T00:00:00Z",
    "id": "2",
    "text": "[Changed] Dolor hic set amit"
  }
}`)}

		processed, err := newTestResultProcessor(noopStore, false).process(context.Background(), processResultParams{
			cacheKey:         "abc123",
			cachedResult:     &CachedResult{Data: mustObject(t, processorCachedDashboard)},
			nextResults:      []*executionResult{nextResult},
			originalDocument: parseDocumentForTest(t, `query { a }`),
		})
		if err != nil {
			t.Fatal(err)
		}

		expectSameJSON(t, processedAt(t, processed.data, "dashboard", "topActivity"), `{
  "__typename": "Activity",
  "description": "Lorem ipsum dolor",
  "id": 2,
  "title": "Lorem ipsum"
}`)
		expectSameJSON(t, processedAt(t, processed.data, "dashboard", "latestUpdates", 0), `{
  "__typename": "UpdateInfo",
  "date": "2023-12-04T00:00:00Z",
  "id": "1",
  "text": "Lorem ipsum"
}`)
		expectSameJSON(t, processedAt(t, processed.data, "dashboard", "latestUpdates", 1), `{
  "__typename": "UpdateInfo",
  "date": "2023-12-03T00:00:00Z",
  "id": "2",
  "text": "[Changed] Dolor hic set amit"
}`)
	})

	t.Run("maps batch cache resolver results into main data tree", func(t *testing.T) {
		cached := `{
  "dashboard": {
    "latestUpdates": [
      {
        "__entityCacheId": "1",
        "__entityCacheTypeName": "UpdateInfo",
        "__typename": "UpdateInfo",
        "date": "2023-12-04T00:00:00Z",
        "id": "1",
        "text": "Lorem ipsum"
      },
      {
        "__entityCacheId": "2",
        "__entityCacheTypeName": "UpdateInfo",
        "__typename": "UpdateInfo",
        "date": "2023-12-03T00:00:00Z",
        "id": "2",
        "text": "Dolor hic set amit"
      },
      {
        "__entityCacheId": "3",
        "__entityCacheTypeName": "UpdateInfo",
        "__typename": "UpdateInfo",
        "date": "2023-12-03T00:00:00Z",
        "id": "3",
        "text": "Foo bar"
      }
    ],
    "topActivity": {
      "__entityCacheId": 2,
      "__entityCacheTypeName": "Activity",
      "__typename": "Activity",
      "description": "Lorem ipsum dolor",
      "id": 2,
      "title": "Lorem ipsum"
    }
  }
}`
		nextResult := &executionResult{data: mustObject(t, `{
  "`+getCacheResolverAlias("Query.dashboard.latestUpdates", 0)+`": [
    {
      "__entityCacheId": "1",
      "__entityCacheTypeName": "UpdateInfo",
      "__typename": "UpdateInfo",
      "date": "2023-12-04T00:00:00Z",
      "id": "1",
      "text": "[Changed] Lorem ipsum"
    },
    {
      "__entityCacheId": "3",
      "__entityCacheTypeName": "UpdateInfo",
      "__typename": "UpdateInfo",
      "date": "2023-12-03T00:00:00Z",
      "id": "3",
      "text": "[Changed] Foo bar"
    }
  ]
}`)}

		processed, err := newTestResultProcessor(noopStore, false).process(context.Background(), processResultParams{
			cacheKey:         "abc123",
			cachedResult:     &CachedResult{Data: mustObject(t, cached)},
			nextResults:      []*executionResult{nextResult},
			originalDocument: parseDocumentForTest(t, `query { a }`),
		})
		if err != nil {
			t.Fatal(err)
		}

		expectSameJSON(t, processedAt(t, processed.data, "dashboard", "topActivity"), `{
  "__typename": "Activity",
  "description": "Lorem ipsum dolor",
  "id": 2,
  "title": "Lorem ipsum"
}`)
		expectSameJSON(t, processedAt(t, processed.data, "dashboard", "latestUpdates", 0), `{
  "__typename": "UpdateInfo",
  "date": "2023-12-04T00:00:00Z",
  "id": "1",
  "text": "[Changed] Lorem ipsum"
}`)
		expectSameJSON(t, processedAt(t, processed.data, "dashboard", "latestUpdates", 1), `{
  "__typename": "UpdateInfo",
  "date": "2023-12-03T00:00:00Z",
  "id": "2",
  "text": "Dolor hic set amit"
}`)
		expectSameJSON(t, processedAt(t, processed.data, "dashboard", "latestUpdates", 2), `{
  "__typename": "UpdateInfo",
  "date": "2023-12-03T00:00:00Z",
  "id": "3",
  "text": "[Changed] Foo bar"
}`)
	})
}

func describeEntities(entities []EntityWithLocation) string {
	out := ""
	for _, entity := range entities {
		id := "<nil>"
		if entity.Entity.ID != nil {
			id = *entity.Entity.ID
		}
		out += "\n  " + entity.Entity.Typename + "#" + id + " at " + DataPathToStr(entity.Path)
	}
	return out
}
