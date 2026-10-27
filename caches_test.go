package mangrove_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/ourstudio-se/mangrove-go/internal/testexec"
	"github.com/ourstudio-se/mangrove-go/memorycache"
	"github.com/ourstudio-se/mangrove-go/rediscache"
)

func members(pairs ...any) []mangrove.Member {
	var out []mangrove.Member
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, mangrove.Member{Member: pairs[i].(string), TTL: pairs[i+1].(time.Duration)})
	}
	return out
}

func must[T any](t testing.TB) func(T, error) T {
	return func(value T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
}

func noErr(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestCacheContract(t *testing.T) {
	ctx := context.Background()
	minute := time.Minute

	forEachBackend(t, func(t *testing.T, b backend) {
		isMember := func(cache mangrove.Cache, key, member string) bool {
			t.Helper()
			return must[bool](t)(cache.IsSetMember(ctx, key, member))
		}

		t.Run("values", func(t *testing.T) {
			cache := b.newCache(t, nil)
			_, found, err := cache.Get(ctx, "k")
			noErr(t, err)
			expectEqual(t, found, false)
			expectEqual(t, must[bool](t)(cache.Exists(ctx, "k")), false)
			noErr(t, cache.Set(ctx, "k", "v", minute))
			value, found, err := cache.Get(ctx, "k")
			noErr(t, err)
			expectEqual(t, [2]any{value, found}, [2]any{"v", true})
			expectEqual(t, must[bool](t)(cache.Exists(ctx, "k")), true)
			noErr(t, cache.Clear(ctx, []string{"k"}))
			_, found, _ = cache.Get(ctx, "k")
			expectEqual(t, found, false)
		})

		t.Run("set members expire individually", func(t *testing.T) {
			cache := b.newCache(t, nil)
			noErr(t, cache.AddMembersToSet(ctx, "s", members("short", 50*time.Millisecond, "long", minute), mangrove.AddMembersOptions{}))
			got := must[[]string](t)(cache.GetSetMembers(ctx, "s"))
			slices.Sort(got)
			expectEqual(t, got, []string{"long", "short"})
			time.Sleep(80 * time.Millisecond)
			expectEqual(t, must[[]string](t)(cache.GetSetMembers(ctx, "s")), []string{"long"})
			expectEqual(t, isMember(cache, "s", "short"), false)
			expectEqual(t, isMember(cache, "s", "long"), true)
			expectEqual(t, must[[]bool](t)(cache.AreSetMembers(ctx, []mangrove.SetMember{
				{Key: "s", Member: "short"}, {Key: "s", Member: "long"}, {Key: "other", Member: "long"},
			})), []bool{false, true, false})
		})

		t.Run("OnlyNew never extends existing members", func(t *testing.T) {
			cache := b.newCache(t, nil)
			noErr(t, cache.AddMembersToSet(ctx, "s", members("m", 50*time.Millisecond), mangrove.AddMembersOptions{}))
			noErr(t, cache.AddMembersToSet(ctx, "s", members("m", minute), mangrove.AddMembersOptions{OnlyNew: true}))
			time.Sleep(80 * time.Millisecond)
			expectEqual(t, isMember(cache, "s", "m"), false)
		})

		t.Run("InheritFrom carries over an existing member", func(t *testing.T) {
			cache := b.newCache(t, nil)
			noErr(t, cache.AddMembersToSet(ctx, "s", members("old", 50*time.Millisecond), mangrove.AddMembersOptions{}))
			noErr(t, cache.AddMembersToSet(ctx, "s", members("new", minute), mangrove.AddMembersOptions{InheritFrom: "old"}))
			expectEqual(t, isMember(cache, "s", "old"), false)
			expectEqual(t, isMember(cache, "s", "new"), true)
			// The new member expires with the old one.
			time.Sleep(80 * time.Millisecond)
			expectEqual(t, isMember(cache, "s", "new"), false)
			// Nothing to inherit from: nothing is added.
			noErr(t, cache.AddMembersToSet(ctx, "s", members("newer", minute), mangrove.AddMembersOptions{InheritFrom: "missing"}))
			expectEqual(t, isMember(cache, "s", "newer"), false)
		})

		t.Run("guarded insertions are skipped after a newer invalidation", func(t *testing.T) {
			cache := b.newCache(t, nil)
			epoch := must[int64](t)(cache.GetEpoch(ctx))

			noErr(t, cache.Invalidate(ctx, mangrove.InvalidateParams{Keys: []string{"Item:1"}, Markers: []string{"Item:1"}}))
			expectEqual(t, must[int64](t)(cache.GetEpoch(ctx)), epoch+1)

			noErr(t, cache.AddMembersToSet(ctx, "Item:1", members("op", minute), mangrove.AddMembersOptions{
				Guard: &mangrove.InvalidationGuard{Epoch: epoch, Markers: []string{"Item:1", "Item"}},
			}))
			expectEqual(t, isMember(cache, "Item:1", "op"), false)

			// Unrelated markers don't interfere, and newer epochs are fine.
			noErr(t, cache.AddMembersToSet(ctx, "Item:2", members("op", minute), mangrove.AddMembersOptions{
				Guard: &mangrove.InvalidationGuard{Epoch: epoch, Markers: []string{"Item:2", "Item"}},
			}))
			noErr(t, cache.AddMembersToSet(ctx, "Item:1", members("op", minute), mangrove.AddMembersOptions{
				Guard: &mangrove.InvalidationGuard{Epoch: epoch + 1, Markers: []string{"Item:1", "Item"}},
			}))
			expectEqual(t, isMember(cache, "Item:2", "op"), true)
			expectEqual(t, isMember(cache, "Item:1", "op"), true)
		})

		t.Run("invalidating an index deletes its members", func(t *testing.T) {
			cache := b.newCache(t, nil)
			noErr(t, cache.AddMembersToSet(ctx, "Item:1", members("op", minute), mangrove.AddMembersOptions{}))
			noErr(t, cache.AddMembersToSet(ctx, "ItemGroup:1", members("op", minute), mangrove.AddMembersOptions{}))
			noErr(t, cache.AddMembersToSet(ctx, "typeindex:Item", members("Item:1", minute), mangrove.AddMembersOptions{}))

			noErr(t, cache.Invalidate(ctx, mangrove.InvalidateParams{
				Indexes: []string{"typeindex:Item"},
				Keys:    []string{"Item"},
				Markers: []string{"Item"},
			}))

			expectEqual(t, isMember(cache, "Item:1", "op"), false)
			expectEqual(t, isMember(cache, "ItemGroup:1", "op"), true)
			expectEqual(t, must[bool](t)(cache.Exists(ctx, "typeindex:Item")), false)
		})

		t.Run("pipes apply all operations together", func(t *testing.T) {
			cache := b.newCache(t, nil)
			pipe := must[mangrove.CachePipe](t)(cache.GetPipe(nil))
			noErr(t, pipe.Set(ctx, "a", "1", minute))
			noErr(t, pipe.AddMembersToSet(ctx, "s", members("m", minute), mangrove.AddMembersOptions{}))
			_, found, _ := cache.Get(ctx, "a")
			expectEqual(t, found, false)
			noErr(t, pipe.Execute(ctx))
			value, _, _ := cache.Get(ctx, "a")
			expectEqual(t, value, "1")
			expectEqual(t, isMember(cache, "s", "m"), true)
		})

		t.Run("conditional pipes don't overwrite writes from later epochs", func(t *testing.T) {
			cache := b.newCache(t, nil)
			condition := func(epoch int64) *mangrove.StoreCondition {
				return &mangrove.StoreCondition{Epoch: epoch, Key: "op:epoch", TTL: minute}
			}

			newer := must[mangrove.CachePipe](t)(cache.GetPipe(condition(2)))
			noErr(t, newer.Set(ctx, "result", "new", minute))
			noErr(t, newer.AddMembersToSet(ctx, "Item:1", members("op", minute), mangrove.AddMembersOptions{}))
			noErr(t, newer.Execute(ctx))

			older := must[mangrove.CachePipe](t)(cache.GetPipe(condition(1)))
			noErr(t, older.Set(ctx, "result", "old", minute))
			noErr(t, older.Clear(ctx, []string{"Item:1"}))
			noErr(t, older.AddMembersToSet(ctx, "Item:2", members("op", minute), mangrove.AddMembersOptions{}))
			noErr(t, older.Execute(ctx))

			value, _, _ := cache.Get(ctx, "result")
			expectEqual(t, value, "new")
			expectEqual(t, isMember(cache, "Item:1", "op"), true)
			expectEqual(t, isMember(cache, "Item:2", "op"), false)

			same := must[mangrove.CachePipe](t)(cache.GetPipe(condition(2)))
			noErr(t, same.Set(ctx, "result", "same epoch", minute))
			noErr(t, same.Execute(ctx))
			value, _, _ = cache.Get(ctx, "result")
			expectEqual(t, value, "same epoch")
		})

		t.Run("removing members", func(t *testing.T) {
			cache := b.newCache(t, nil)
			noErr(t, cache.AddMembersToSet(ctx, "s", members("a", minute, "b", minute), mangrove.AddMembersOptions{}))
			noErr(t, cache.RemoveMembersFromSet(ctx, "s", []string{"a"}))
			expectEqual(t, must[[]string](t)(cache.GetSetMembers(ctx, "s")), []string{"b"})
		})

		t.Run("rejects non-positive TTLs", func(t *testing.T) {
			cache := b.newCache(t, nil)
			if cache.Set(ctx, "k", "v", 0) == nil {
				t.Fatal("expected an error")
			}
			if cache.AddMembersToSet(ctx, "s", members("m", -time.Millisecond), mangrove.AddMembersOptions{}) == nil {
				t.Fatal("expected an error")
			}
			if _, err := cache.GetPipe(&mangrove.StoreCondition{Key: "k"}); err == nil {
				t.Fatal("expected an error")
			}
		})
	})
}

func TestMemoryCacheSweeps(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	cache := memorycache.New(memorycache.Options{Now: clk.Now, SweepInterval: 10})
	for i := 0; i < 9; i++ {
		noErr(t, cache.Set(ctx, "k"+string(rune('0'+i)), "v", 100*time.Millisecond))
	}
	noErr(t, cache.Invalidate(ctx, mangrove.InvalidateParams{Markers: []string{"m"}}))
	expectEqual(t, cache.Size(), 10)
	clk.Set(time.Hour)
	noErr(t, cache.Set(ctx, "fresh", "v", 100*time.Millisecond))
	expectEqual(t, cache.Size(), 1)
}

func TestRedisCacheSpecifics(t *testing.T) {
	ctx := context.Background()
	minute := time.Minute

	setup := func(t *testing.T) {
		redisMu.Lock()
		t.Cleanup(redisMu.Unlock)
	}

	t.Run("key prefixes namespace every key", func(t *testing.T) {
		client := newRedisClient(t)
		setup(t)
		noErr(t, client.FlushAll(ctx).Err())
		cache := rediscache.New(rediscache.Options{Client: client, KeyPrefix: "app1:"})

		noErr(t, cache.Set(ctx, "k", "v", minute))
		noErr(t, cache.AddMembersToSet(ctx, "typeindex:T", members("T:1", minute), mangrove.AddMembersOptions{}))
		noErr(t, cache.AddMembersToSet(ctx, "T:1", members("op", minute), mangrove.AddMembersOptions{}))
		noErr(t, cache.Invalidate(ctx, mangrove.InvalidateParams{Indexes: []string{"typeindex:T"}, Markers: []string{"T"}}))

		keys := must[[]string](t)(client.Keys(ctx, "*").Result())
		slices.Sort(keys)
		expectEqual(t, keys, []string{"app1::mangrove:epoch", "app1::mangrove:invalidated:T", "app1:k"})
	})

	t.Run("plain sets live as long as their longest member", func(t *testing.T) {
		client := newRedisClient(t)
		setup(t)
		noErr(t, client.FlushAll(ctx).Err())
		cache := rediscache.New(rediscache.Options{Client: client, PlainSets: true})

		noErr(t, cache.AddMembersToSet(ctx, "s", members("a", minute), mangrove.AddMembersOptions{}))
		noErr(t, cache.AddMembersToSet(ctx, "s", members("b", 100*time.Millisecond), mangrove.AddMembersOptions{}))
		if ttl := must[time.Duration](t)(client.PTTL(ctx, "s").Result()); ttl < 50*time.Second {
			t.Fatalf("expected the set to live for the longest member, got %s", ttl)
		}
		expectEqual(t, must[bool](t)(cache.IsSetMember(ctx, "s", "b")), true)
	})

	t.Run("failed writes are reported", func(t *testing.T) {
		client := newRedisClient(t)
		setup(t)
		noErr(t, client.FlushAll(ctx).Err())
		cache := rediscache.New(rediscache.Options{Client: client})
		noErr(t, client.Set(ctx, "s", "not a set", 0).Err())

		err := cache.AddMembersToSet(ctx, "s", members("m", minute), mangrove.AddMembersOptions{})
		if err == nil || !strings.Contains(err.Error(), "WRONGTYPE") {
			t.Fatalf("expected a WRONGTYPE error, got %v", err)
		}
	})

	t.Run("compression is applied to stored values", func(t *testing.T) {
		client := newRedisClient(t)
		setup(t)
		noErr(t, client.FlushAll(ctx).Err())
		reverse := func(data []byte) ([]byte, error) {
			out := bytes.Clone(data)
			slices.Reverse(out)
			return out, nil
		}
		cache := rediscache.New(rediscache.Options{Client: client, Compress: reverse, Decompress: reverse})

		noErr(t, cache.Set(ctx, "k", "abc", minute))
		expectEqual(t, must[string](t)(client.Get(ctx, "k").Result()), "cba")
		value, _, err := cache.Get(ctx, "k")
		noErr(t, err)
		expectEqual(t, value, "abc")
		_, value, _, err = cache.GetWithEpoch(ctx, "k")
		noErr(t, err)
		expectEqual(t, value, "abc")
	})

	for _, plain := range []bool{false, true} {
		t.Run("type invalidation with a key prefix and script flushes", func(t *testing.T) {
			client := newRedisClient(t)
			setup(t)
			noErr(t, client.FlushAll(ctx).Err())
			cache := rediscache.New(rediscache.Options{Client: client, KeyPrefix: "pfx:", PlainSets: plain})
			v := 1
			h := newHarness(t, harnessOptions{
				cache:     cache,
				resolvers: testexec.Resolvers{"Query": {"item": fn0(func() any { return obj{"id": "1", "v": v} })}},
				typeDefs:  `type Query { item: Item! } type Item { id: ID! v: Int! }`,
			})
			q := `{ item { id v } }`
			h.run(q)
			noErr(t, client.ScriptFlush(ctx).Err())
			v = 2
			h.invalidate(mangrove.EntityType("Item"))
			expectEqual(t, h.run(q).path(t, "item", "v"), 2.0)
			noErr(t, client.ScriptFlush(ctx).Err())
			v = 3
			h.invalidate(mangrove.Entity("Item", "1"))
			expectEqual(t, h.run(q).path(t, "item", "v"), 3.0)
			r3 := h.run(q)
			h.run(q)
			h.run(q)
			expectLen(t, r3.executed, 0)
			h.expectNoErrors()
		})
	}
}
