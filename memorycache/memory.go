// Package memorycache is an in-process cache with the same semantics as the
// Redis cache. Useful for tests and single-process deployments.
package memorycache

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ourstudio-se/mangrove-go"
)

// Options configure the memory cache.
type Options struct {
	// InvalidationWindow is how long invalidations are remembered for, to
	// reject results of requests that started before them. Must exceed the
	// longest request. Defaults to 10 minutes.
	InvalidationWindow time.Duration
	// Now is the clock, for tests. Defaults to time.Now.
	Now func() time.Time
	// SweepInterval: expired entries are swept after this many writes.
	// Defaults to 1000.
	SweepInterval int
}

type expiring[T any] struct {
	expiresAt int64
	value     T
}

// Cache is an in-process cache.
type Cache struct {
	mu                 sync.Mutex
	values             map[string]expiring[string]
	sets               map[string]expiring[map[string]int64]
	markers            map[string]expiring[int64]
	claims             map[string]expiring[int64]
	epoch              int64
	invalidationWindow int64
	now                func() time.Time
	sweepInterval      int
	writes             int
}

var _ mangrove.Cache = (*Cache)(nil)

// New creates a memory cache.
func New(options Options) *Cache {
	c := &Cache{
		values:             map[string]expiring[string]{},
		sets:               map[string]expiring[map[string]int64]{},
		markers:            map[string]expiring[int64]{},
		claims:             map[string]expiring[int64]{},
		invalidationWindow: (10 * time.Minute).Milliseconds(),
		now:                options.Now,
		sweepInterval:      options.SweepInterval,
	}
	if options.InvalidationWindow > 0 {
		c.invalidationWindow = options.InvalidationWindow.Milliseconds()
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sweepInterval <= 0 {
		c.sweepInterval = 1000
	}
	return c
}

func (c *Cache) nowMs() int64 {
	return c.now().UnixMilli()
}

func live[T any](m map[string]expiring[T], key string, now int64) (T, bool) {
	entry, ok := m[key]
	if !ok {
		var zero T
		return zero, false
	}
	if entry.expiresAt <= now {
		delete(m, key)
		var zero T
		return zero, false
	}
	return entry.value, true
}

func (c *Cache) liveMembers(key string) map[string]int64 {
	t := c.nowMs()
	members, ok := live(c.sets, key, t)
	if !ok {
		return nil
	}
	for member, expiresAt := range members {
		if expiresAt <= t {
			delete(members, member)
		}
	}
	if len(members) == 0 {
		delete(c.sets, key)
		return nil
	}
	return members
}

// Entries expire lazily when read; sweep regularly so that entries that are
// never read again don't accumulate.
func (c *Cache) sweep() {
	t := c.nowMs()
	for key := range c.values {
		live(c.values, key, t)
	}
	for key := range c.markers {
		live(c.markers, key, t)
	}
	for key := range c.claims {
		live(c.claims, key, t)
	}
	for key := range c.sets {
		c.liveMembers(key)
	}
}

func (c *Cache) countWrite() {
	c.writes++
	if c.writes%c.sweepInterval == 0 {
		c.sweep()
	}
}

func (c *Cache) deleteKey(key string) {
	delete(c.values, key)
	delete(c.sets, key)
}

func (c *Cache) addMembersToSet(key string, members []mangrove.Member, options mangrove.AddMembersOptions) error {
	c.countWrite()

	if options.Guard != nil {
		for _, marker := range options.Guard.Markers {
			if invalidatedAt, ok := live(c.markers, marker, c.nowMs()); ok && invalidatedAt > options.Guard.Epoch {
				return nil
			}
		}
	}

	t := c.nowMs()
	set := c.liveMembers(key)
	if set == nil {
		set = map[string]int64{}
	}

	inheritedExpiry := int64(math.MaxInt64)
	if options.InheritFrom != "" {
		inherited, ok := set[options.InheritFrom]
		if !ok {
			return nil
		}
		inheritedExpiry = inherited
		delete(set, options.InheritFrom)
	}

	for _, member := range members {
		if member.TTL <= 0 {
			return &mangrove.CacheValidationError{Message: fmt.Sprintf("Member TTL must be positive, got %s", member.TTL)}
		}
		expiresAt := min(t+member.TTL.Milliseconds(), inheritedExpiry)
		current, ok := set[member.Member]
		if !ok || (!options.OnlyNew && expiresAt > current) {
			set[member.Member] = expiresAt
		}
	}

	if len(set) == 0 {
		delete(c.sets, key)
		return nil
	}

	var setExpiresAt int64
	for _, memberExpiresAt := range set {
		setExpiresAt = max(setExpiresAt, memberExpiresAt)
	}

	c.sets[key] = expiring[map[string]int64]{expiresAt: setExpiresAt, value: set}
	return nil
}

func (c *Cache) clear(keys []string) {
	for _, key := range keys {
		c.deleteKey(key)
	}
}

func (c *Cache) removeMembersFromSet(key string, members []string) {
	set := c.liveMembers(key)
	if set == nil {
		return
	}
	for _, member := range members {
		delete(set, member)
	}
	if len(set) == 0 {
		delete(c.sets, key)
	}
}

func (c *Cache) set(key, data string, ttl time.Duration) error {
	if ttl <= 0 {
		return &mangrove.CacheValidationError{Message: fmt.Sprintf("TTL must be positive, got %s", ttl)}
	}
	c.countWrite()
	c.values[key] = expiring[string]{expiresAt: c.nowMs() + ttl.Milliseconds(), value: data}
	return nil
}

// AddMembersToSet adds members to a set.
func (c *Cache) AddMembersToSet(_ context.Context, key string, members []mangrove.Member, options mangrove.AddMembersOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addMembersToSet(key, members, options)
}

// Clear deletes keys.
func (c *Cache) Clear(_ context.Context, keys []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clear(keys)
	return nil
}

// RemoveMembersFromSet removes members from a set.
func (c *Cache) RemoveMembersFromSet(_ context.Context, key string, members []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeMembersFromSet(key, members)
	return nil
}

// Set sets a value.
func (c *Cache) Set(_ context.Context, key, data string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.set(key, data, ttl)
}

// Exists reports whether a key exists.
func (c *Cache) Exists(_ context.Context, key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := live(c.values, key, c.nowMs()); ok {
		return true, nil
	}
	return c.liveMembers(key) != nil, nil
}

// Get returns a value.
func (c *Cache) Get(_ context.Context, key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := live(c.values, key, c.nowMs())
	return value, ok, nil
}

// GetEpoch returns the current invalidation epoch.
func (c *Cache) GetEpoch(context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch, nil
}

// GetWithEpoch returns the current invalidation epoch and a value.
func (c *Cache) GetWithEpoch(_ context.Context, key string) (int64, string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := live(c.values, key, c.nowMs())
	return c.epoch, value, ok, nil
}

// GetSetMembers returns the live members of a set.
func (c *Cache) GetSetMembers(_ context.Context, key string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	set := c.liveMembers(key)
	members := make([]string, 0, len(set))
	for member := range set {
		members = append(members, member)
	}
	return members, nil
}

// IsSetMember reports whether member is a live member of a set.
func (c *Cache) IsSetMember(_ context.Context, key, member string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.liveMembers(key)[member]
	return ok, nil
}

// AreSetMembers checks several memberships.
func (c *Cache) AreSetMembers(_ context.Context, members []mangrove.SetMember) ([]bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]bool, len(members))
	for i, query := range members {
		_, out[i] = c.liveMembers(query.Key)[query.Member]
	}
	return out, nil
}

// Size returns the number of stored entries, including expired ones not yet
// swept.
func (c *Cache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.values) + len(c.sets) + len(c.markers) + len(c.claims)
}

// Invalidate bumps the epoch, records it for every marker and deletes keys.
func (c *Cache) Invalidate(_ context.Context, params mangrove.InvalidateParams) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	expiresAt := c.nowMs() + c.invalidationWindow
	for _, marker := range params.Markers {
		c.markers[marker] = expiring[int64]{expiresAt: expiresAt, value: c.epoch}
	}
	for _, index := range params.Indexes {
		for member := range c.liveMembers(index) {
			c.deleteKey(member)
		}
		c.deleteKey(index)
	}
	for _, key := range params.Keys {
		c.deleteKey(key)
	}
	return nil
}

// GetPipe returns a pipe whose mutations apply atomically.
func (c *Cache) GetPipe(condition *mangrove.StoreCondition) (mangrove.CachePipe, error) {
	if condition != nil && condition.TTL <= 0 {
		return nil, &mangrove.CacheValidationError{Message: fmt.Sprintf("Condition TTL must be positive, got %s", condition.TTL)}
	}
	return &pipe{cache: c, condition: condition}, nil
}

type pipe struct {
	cache     *Cache
	condition *mangrove.StoreCondition
	queue     []func() error
}

func (p *pipe) AddMembersToSet(_ context.Context, key string, members []mangrove.Member, options mangrove.AddMembersOptions) error {
	p.queue = append(p.queue, func() error { return p.cache.addMembersToSet(key, members, options) })
	return nil
}

func (p *pipe) Clear(_ context.Context, keys []string) error {
	p.queue = append(p.queue, func() error { p.cache.clear(keys); return nil })
	return nil
}

func (p *pipe) RemoveMembersFromSet(_ context.Context, key string, members []string) error {
	p.queue = append(p.queue, func() error { p.cache.removeMembersFromSet(key, members); return nil })
	return nil
}

func (p *pipe) Set(_ context.Context, key, data string, ttl time.Duration) error {
	p.queue = append(p.queue, func() error { return p.cache.set(key, data, ttl) })
	return nil
}

func (p *pipe) Execute(context.Context) error {
	c := p.cache
	c.mu.Lock()
	defer c.mu.Unlock()

	queue := p.queue
	p.queue = nil

	if p.condition != nil {
		if claimed, ok := live(c.claims, p.condition.Key, c.nowMs()); ok && claimed > p.condition.Epoch {
			return nil
		}
		c.claims[p.condition.Key] = expiring[int64]{expiresAt: c.nowMs() + p.condition.TTL.Milliseconds(), value: p.condition.Epoch}
	}

	var firstErr error
	for _, operation := range queue {
		if err := operation(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
