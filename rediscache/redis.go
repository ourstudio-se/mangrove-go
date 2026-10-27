// Package rediscache stores Mangrove's cache in Redis. Every write is a Lua
// script, so that writes can be made conditional and applied atomically.
// Redis Cluster is not supported.
package rediscache

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ourstudio-se/mangrove-go"
	"github.com/redis/go-redis/v9"
)

// Options configure the Redis cache.
type Options struct {
	Client redis.UniversalClient
	// PlainSets stores set members in plain sets instead of sorted sets. With
	// plain sets, a set and all of its members live as long as the
	// longest-lived member, so per-entity TTLs aren't honoured. Don't switch
	// between modes on a populated database.
	PlainSets bool
	// Compress and Decompress transform stored values.
	Compress   func([]byte) ([]byte, error)
	Decompress func([]byte) ([]byte, error)
	// InvalidationWindow is how long invalidations are remembered for, to
	// reject results of requests that started before them. Must exceed the
	// longest request. Defaults to 10 minutes.
	InvalidationWindow time.Duration
	// KeyPrefix prefixes every key written by Mangrove.
	KeyPrefix string
	// Now is the clock, for tests. Defaults to time.Now.
	Now func() time.Time
}

/*
 * Writes in a conditional pipe only apply if the pipe's claim succeeded,
 * i.e. if the condition key still holds the pipe's epoch.
 *
 * KEYS[1]     condition key
 * ARGV[1]     epoch, or "" for unconditional writes
 */
const conditionCheck = `
if ARGV[1] ~= "" and redis.call("GET", KEYS[1]) ~= ARGV[1] then
  return 0
end
`

/*
 * KEYS[1]     condition key
 * ARGV[1]     epoch
 * ARGV[2]     TTL (ms)
 */
const claimScript = `
local current = redis.call("GET", KEYS[1])
if current and tonumber(current) > tonumber(ARGV[1]) then
  return 0
end
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[2])
return 1
`

/*
 * KEYS[1]     condition key
 * KEYS[2]     key
 * ARGV[1]     condition epoch, or ""
 * ARGV[2]     "set", "del" or "rem"
 * ARGV[3]     value (set), or "z"/"s" (rem)
 * ARGV[4..]   TTL in ms (set), or members (rem)
 */
const writeScript = conditionCheck + `
if ARGV[2] == "set" then
  redis.call("SET", KEYS[2], ARGV[3], "PX", ARGV[4])
elseif ARGV[2] == "rem" then
  for i = 4, #ARGV do
    if ARGV[3] == "z" then
      redis.call("ZREM", KEYS[2], ARGV[i])
    else
      redis.call("SREM", KEYS[2], ARGV[i])
    end
  end
else
  redis.call("DEL", KEYS[2])
end
return 1
`

/*
 * Adds members to any number of sets in one call.
 *
 * KEYS[1]     condition key
 * KEYS[2..]   per set: the set key, followed by its invalidation marker keys
 * ARGV[1]     condition epoch, or ""
 * ARGV[2]     now (ms)
 * ARGV[3]     "z" (sorted sets) or "s" (sets)
 * ARGV[4]     number of sets
 * ARGV[5..]   per set: marker count, guard epoch (or ""), "1" to only add new
 *             members (never extend existing ones), member to inherit from
 *             (or ""), member count, then expiresAt, member pairs
 */
const addMembersScript = conditionCheck + `
local now = tonumber(ARGV[2])
local sorted = ARGV[3] == "z"
local k = 2
local a = 5
for _ = 1, tonumber(ARGV[4]) do
  local key = KEYS[k]
  local markerCount = tonumber(ARGV[a])
  local guard = ARGV[a + 1]
  local onlyNew = ARGV[a + 2] == "1"
  local inheritFrom = ARGV[a + 3]
  local memberCount = tonumber(ARGV[a + 4])
  a = a + 5
  local skip = false
  local inheritedExpiresAt = nil
  if guard ~= "" then
    local epoch = tonumber(guard)
    for i = k + 1, k + markerCount do
      local invalidatedAt = redis.call("GET", KEYS[i])
      if invalidatedAt and tonumber(invalidatedAt) > epoch then
        skip = true
        break
      end
    end
  end
  if not skip and inheritFrom ~= "" then
    if sorted then
      local score = redis.call("ZSCORE", key, inheritFrom)
      if score and tonumber(score) > now then
        inheritedExpiresAt = tonumber(score)
        redis.call("ZREM", key, inheritFrom)
      else
        skip = true
      end
    elseif redis.call("SREM", key, inheritFrom) == 0 then
      skip = true
    end
  end
  if not skip then
    local maxExpiresAt = 0
    if sorted then
      redis.call("ZREMRANGEBYSCORE", key, "-inf", now)
    end
    for i = a, a + 2 * memberCount - 1, 2 do
      local expiresAt = tonumber(ARGV[i])
      if inheritedExpiresAt and inheritedExpiresAt < expiresAt then
        expiresAt = inheritedExpiresAt
      end
      local member = ARGV[i + 1]
      if sorted then
        local current = redis.call("ZSCORE", key, member)
        if not current then
          redis.call("ZADD", key, expiresAt, member)
        elseif not onlyNew and tonumber(current) < expiresAt then
          redis.call("ZADD", key, expiresAt, member)
        end
      else
        redis.call("SADD", key, member)
      end
      if expiresAt > maxExpiresAt then
        maxExpiresAt = expiresAt
      end
    end
    local pttl = redis.call("PTTL", key)
    if pttl ~= -2 and (pttl < 0 or now + pttl < maxExpiresAt) then
      redis.call("PEXPIREAT", key, maxExpiresAt)
    end
  end
  k = k + 1 + markerCount
  a = a + 2 * memberCount
end
return 1
`

/*
 * KEYS[1]                epoch key
 * KEYS[2..ARGV[1]+1]     invalidation marker keys
 * KEYS[ARGV[1]+2..]      keys to delete
 * ARGV[1]                number of marker keys
 * ARGV[2]                marker TTL (ms)
 * ARGV[3]                key prefix
 * ARGV[4]                number of index keys (the last keys to delete)
 */
const invalidateScript = `
local epoch = redis.call("INCR", KEYS[1])
local markerCount = tonumber(ARGV[1])
for i = 2, markerCount + 1 do
  redis.call("SET", KEYS[i], epoch, "PX", ARGV[2])
end
local indexCount = tonumber(ARGV[4])
local firstIndex = #KEYS - indexCount + 1
for i = firstIndex, #KEYS do
  local kind = redis.call("TYPE", KEYS[i])["ok"]
  local members
  if kind == "zset" then
    members = redis.call("ZRANGE", KEYS[i], 0, -1)
  elseif kind == "set" then
    members = redis.call("SMEMBERS", KEYS[i])
  else
    members = {}
  end
  for _, member in ipairs(members) do
    redis.call("DEL", ARGV[3] .. member)
  end
end
for i = markerCount + 2, #KEYS do
  redis.call("DEL", KEYS[i])
end
return epoch
`

var (
	claim      = redis.NewScript(claimScript)
	write      = redis.NewScript(writeScript)
	addMembers = redis.NewScript(addMembersScript)
	invalidate = redis.NewScript(invalidateScript)
	scripts    = []*redis.Script{claim, write, addMembers, invalidate}
)

// Cache is the Redis cache.
type Cache struct {
	client             redis.UniversalClient
	sorted             bool
	compress           func([]byte) ([]byte, error)
	decompress         func([]byte) ([]byte, error)
	invalidationWindow int64
	keyPrefix          string
	now                func() time.Time
	epochKey           string
}

var _ mangrove.Cache = (*Cache)(nil)

// New creates a Redis cache.
func New(options Options) *Cache {
	c := &Cache{
		client:             options.Client,
		sorted:             !options.PlainSets,
		compress:           options.Compress,
		decompress:         options.Decompress,
		invalidationWindow: (10 * time.Minute).Milliseconds(),
		keyPrefix:          options.KeyPrefix,
		now:                options.Now,
	}
	if options.InvalidationWindow > 0 {
		c.invalidationWindow = ceilMs(options.InvalidationWindow)
	}
	if c.now == nil {
		c.now = time.Now
	}
	// GraphQL type names can't start with ":", so entity keys can't collide
	// with internal keys.
	c.epochKey = c.k(":mangrove:epoch")
	return c
}

func (c *Cache) k(key string) string {
	return c.keyPrefix + key
}

func (c *Cache) markerKey(marker string) string {
	return c.k(":mangrove:invalidated:" + marker)
}

func (c *Cache) nowMs() int64 {
	return c.now().UnixMilli()
}

func (c *Cache) setMode() string {
	if c.sorted {
		return "z"
	}
	return "s"
}

func ceilMs(d time.Duration) int64 {
	return int64(math.Ceil(float64(d) / float64(time.Millisecond)))
}

type operation func(ctx context.Context, pipe redis.Pipeliner)

type addBatch struct {
	keys []string
	args []any
	sets int
}

// recorder turns mutations into operations on a transaction. Every write is
// a script, so that it can be made conditional.
type recorder struct {
	cache      *Cache
	operations []operation
	batch      *addBatch
	// conditionKey: unconditional writes still need a key in the first slot.
	conditionKey   string
	conditionEpoch string
}

func (c *Cache) newRecorder(condition *mangrove.StoreCondition) *recorder {
	r := &recorder{cache: c, conditionKey: c.epochKey}
	if condition != nil {
		r.conditionKey = c.k(condition.Key)
		r.conditionEpoch = strconv.FormatInt(condition.Epoch, 10)
	}
	return r
}

// recordOther records a mutation other than an addition, which ends the
// current batch of additions; that keeps the order of operations intact.
func (r *recorder) recordOther(op operation) {
	r.batch = nil
	r.operations = append(r.operations, op)
}

func (r *recorder) AddMembersToSet(_ context.Context, key string, members []mangrove.Member, options mangrove.AddMembersOptions) error {
	now := r.cache.nowMs()
	var pairs []any

	for _, member := range members {
		if member.TTL <= 0 {
			return &mangrove.CacheValidationError{Message: fmt.Sprintf("Member TTL must be positive, got %s", member.TTL)}
		}
		pairs = append(pairs, now+ceilMs(member.TTL), member.Member)
	}

	if len(pairs) == 0 {
		return nil
	}

	if r.batch == nil {
		// Consecutive additions are sent as a single script call.
		batch := &addBatch{}
		r.batch = batch
		r.operations = append(r.operations, func(ctx context.Context, pipe redis.Pipeliner) {
			keys := append([]string{r.conditionKey}, batch.keys...)
			args := append([]any{r.conditionEpoch, r.cache.nowMs(), r.cache.setMode(), batch.sets}, batch.args...)
			addMembers.EvalSha(ctx, pipe, keys, args...)
		})
	}

	var markers []string
	guardEpoch := ""
	if options.Guard != nil {
		for _, marker := range options.Guard.Markers {
			markers = append(markers, r.cache.markerKey(marker))
		}
		guardEpoch = strconv.FormatInt(options.Guard.Epoch, 10)
	}
	onlyNew := "0"
	if options.OnlyNew {
		onlyNew = "1"
	}

	r.batch.sets++
	r.batch.keys = append(r.batch.keys, r.cache.k(key))
	r.batch.keys = append(r.batch.keys, markers...)
	r.batch.args = append(r.batch.args, len(markers), guardEpoch, onlyNew, options.InheritFrom, len(pairs)/2)
	r.batch.args = append(r.batch.args, pairs...)
	return nil
}

func (r *recorder) Clear(_ context.Context, keys []string) error {
	for _, key := range keys {
		prefixed := r.cache.k(key)
		r.recordOther(func(ctx context.Context, pipe redis.Pipeliner) {
			write.EvalSha(ctx, pipe, []string{r.conditionKey, prefixed}, r.conditionEpoch, "del")
		})
	}
	return nil
}

func (r *recorder) RemoveMembersFromSet(_ context.Context, key string, members []string) error {
	if len(members) == 0 {
		return nil
	}
	prefixed := r.cache.k(key)
	args := []any{r.conditionEpoch, "rem", r.cache.setMode()}
	for _, member := range members {
		args = append(args, member)
	}
	r.recordOther(func(ctx context.Context, pipe redis.Pipeliner) {
		write.EvalSha(ctx, pipe, []string{r.conditionKey, prefixed}, args...)
	})
	return nil
}

func (r *recorder) Set(_ context.Context, key, data string, ttl time.Duration) error {
	if ttl <= 0 {
		return &mangrove.CacheValidationError{Message: fmt.Sprintf("TTL must be positive, got %s", ttl)}
	}
	value := []byte(data)
	if r.cache.compress != nil {
		compressed, err := r.cache.compress(value)
		if err != nil {
			return err
		}
		value = compressed
	}
	prefixed := r.cache.k(key)
	r.recordOther(func(ctx context.Context, pipe redis.Pipeliner) {
		write.EvalSha(ctx, pipe, []string{r.conditionKey, prefixed}, r.conditionEpoch, "set", value, ceilMs(ttl))
	})
	return nil
}

type pipe struct {
	*recorder
	condition *mangrove.StoreCondition
}

// GetPipe returns a pipe whose mutations are applied in a transaction.
func (c *Cache) GetPipe(condition *mangrove.StoreCondition) (mangrove.CachePipe, error) {
	if condition != nil && condition.TTL <= 0 {
		return nil, &mangrove.CacheValidationError{Message: fmt.Sprintf("Condition TTL must be positive, got %s", condition.TTL)}
	}
	return &pipe{recorder: c.newRecorder(condition), condition: condition}, nil
}

func (p *pipe) exec(ctx context.Context) error {
	_, err := p.cache.client.TxPipelined(ctx, func(tx redis.Pipeliner) error {
		if p.condition != nil {
			claim.EvalSha(ctx, tx, []string{p.cache.k(p.condition.Key)}, p.condition.Epoch, ceilMs(p.condition.TTL))
		}
		for _, operation := range p.operations {
			operation(ctx, tx)
		}
		return nil
	})
	return err
}

func isNoScript(err error) bool {
	return err != nil && strings.Contains(err.Error(), "NOSCRIPT")
}

// LoadScripts loads Mangrove's scripts into Redis. Scripts are loaded on
// demand; this merely avoids a round trip on first use.
func (c *Cache) LoadScripts(ctx context.Context) error {
	for _, script := range scripts {
		if err := script.Load(ctx, c.client).Err(); err != nil {
			return err
		}
	}
	return nil
}

func (p *pipe) Execute(ctx context.Context) error {
	err := p.exec(ctx)
	if !isNoScript(err) {
		return err
	}
	// Scripts can disappear (SCRIPT FLUSH, failover): load them and retry.
	// Commands of a transaction that fail don't roll back the others, so
	// writes that found their script were applied; running the whole pipe
	// again is safe, as every operation is idempotent and the claim accepts
	// the same epoch again.
	if err := p.cache.LoadScripts(ctx); err != nil {
		return err
	}
	return p.exec(ctx)
}

func (c *Cache) single(ctx context.Context, record func(*recorder) error) error {
	p := &pipe{recorder: c.newRecorder(nil)}
	if err := record(p.recorder); err != nil {
		return err
	}
	return p.Execute(ctx)
}

// AddMembersToSet adds members to a set.
func (c *Cache) AddMembersToSet(ctx context.Context, key string, members []mangrove.Member, options mangrove.AddMembersOptions) error {
	return c.single(ctx, func(r *recorder) error { return r.AddMembersToSet(ctx, key, members, options) })
}

// Clear deletes keys.
func (c *Cache) Clear(ctx context.Context, keys []string) error {
	return c.single(ctx, func(r *recorder) error { return r.Clear(ctx, keys) })
}

// RemoveMembersFromSet removes members from a set.
func (c *Cache) RemoveMembersFromSet(ctx context.Context, key string, members []string) error {
	return c.single(ctx, func(r *recorder) error { return r.RemoveMembersFromSet(ctx, key, members) })
}

// Set sets a value.
func (c *Cache) Set(ctx context.Context, key, data string, ttl time.Duration) error {
	return c.single(ctx, func(r *recorder) error { return r.Set(ctx, key, data, ttl) })
}

// Exists reports whether a key exists.
func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	count, err := c.client.Exists(ctx, c.k(key)).Result()
	return count == 1, err
}

func (c *Cache) decode(value []byte) (string, error) {
	if c.decompress != nil {
		decompressed, err := c.decompress(value)
		if err != nil {
			return "", err
		}
		value = decompressed
	}
	return string(value), nil
}

// Get returns a value.
func (c *Cache) Get(ctx context.Context, key string) (string, bool, error) {
	value, err := c.client.Get(ctx, c.k(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	decoded, err := c.decode(value)
	return decoded, err == nil, err
}

func parseEpoch(value string, err error) (int64, error) {
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(value, 10, 64)
}

// GetEpoch returns the current invalidation epoch.
func (c *Cache) GetEpoch(ctx context.Context) (int64, error) {
	return parseEpoch(c.client.Get(ctx, c.epochKey).Result())
}

// GetWithEpoch reads the epoch, then a value, in a single round trip.
func (c *Cache) GetWithEpoch(ctx context.Context, key string) (int64, string, bool, error) {
	pipe := c.client.Pipeline()
	epochCmd := pipe.Get(ctx, c.epochKey)
	valueCmd := pipe.Get(ctx, c.k(key))
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return 0, "", false, err
	}
	epoch, err := parseEpoch(epochCmd.Result())
	if err != nil {
		return 0, "", false, err
	}
	value, err := valueCmd.Bytes()
	if errors.Is(err, redis.Nil) {
		return epoch, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	decoded, err := c.decode(value)
	if err != nil {
		return 0, "", false, err
	}
	return epoch, decoded, true, nil
}

// GetSetMembers returns the live members of a set.
func (c *Cache) GetSetMembers(ctx context.Context, key string) ([]string, error) {
	if c.sorted {
		return c.client.ZRangeByScore(ctx, c.k(key), &redis.ZRangeBy{
			Min: strconv.FormatInt(c.nowMs()+1, 10),
			Max: "+inf",
		}).Result()
	}
	return c.client.SMembers(ctx, c.k(key)).Result()
}

// IsSetMember reports whether member is a live member of a set.
func (c *Cache) IsSetMember(ctx context.Context, key, member string) (bool, error) {
	results, err := c.AreSetMembers(ctx, []mangrove.SetMember{{Key: key, Member: member}})
	if err != nil {
		return false, err
	}
	return results[0], nil
}

// AreSetMembers checks several memberships in a single round trip.
func (c *Cache) AreSetMembers(ctx context.Context, members []mangrove.SetMember) ([]bool, error) {
	out := make([]bool, len(members))
	if len(members) == 0 {
		return out, nil
	}

	pipe := c.client.Pipeline()
	if c.sorted {
		cmds := make([]*redis.FloatCmd, len(members))
		for i, query := range members {
			cmds[i] = pipe.ZScore(ctx, c.k(query.Key), query.Member)
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}
		now := float64(c.nowMs())
		for i, cmd := range cmds {
			score, err := cmd.Result()
			if errors.Is(err, redis.Nil) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out[i] = score > now
		}
		return out, nil
	}

	cmds := make([]*redis.BoolCmd, len(members))
	for i, query := range members {
		cmds[i] = pipe.SIsMember(ctx, c.k(query.Key), query.Member)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for i, cmd := range cmds {
		out[i] = cmd.Val()
	}
	return out, nil
}

// Invalidate bumps the epoch, records it for every marker and deletes keys.
func (c *Cache) Invalidate(ctx context.Context, params mangrove.InvalidateParams) error {
	keys := []string{c.epochKey}
	for _, marker := range params.Markers {
		keys = append(keys, c.markerKey(marker))
	}
	for _, key := range params.Keys {
		keys = append(keys, c.k(key))
	}
	for _, index := range params.Indexes {
		keys = append(keys, c.k(index))
	}

	return invalidate.Run(ctx, c.client, keys, len(params.Markers), c.invalidationWindow, c.keyPrefix, len(params.Indexes)).Err()
}
