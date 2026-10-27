package mangrove

import (
	"context"
	"time"
)

// CacheValidationError reports invalid arguments to a cache.
type CacheValidationError struct {
	Message string
}

func (e *CacheValidationError) Error() string { return e.Message }

// InvalidationGuard guards a set insertion against invalidations that
// happened after a request started reading from the cache. The insertion is
// skipped if any of the Markers has been invalidated at an epoch later than
// Epoch.
type InvalidationGuard struct {
	Epoch   int64
	Markers []string
}

// AddMembersOptions modify AddMembersToSet.
type AddMembersOptions struct {
	Guard *InvalidationGuard
	// InheritFrom carries over an existing member: the members are only
	// added if InheritFrom is still a (live) member of the set, they expire
	// no later than it does, and it is removed.
	InheritFrom string
	// OnlyNew: only add members that aren't in the set yet; never extend the
	// expiry of an existing member.
	OnlyNew bool
}

// Member is a set member with its time to live.
type Member struct {
	Member string
	TTL    time.Duration
}

// StoreCondition makes a pipe's mutations conditional: they only apply if no
// pipe with the same Key and a later Epoch has been executed before (within
// TTL).
type StoreCondition struct {
	Epoch int64
	Key   string
	TTL   time.Duration
}

// InvalidateParams are the parameters of Cache.Invalidate.
type InvalidateParams struct {
	// Indexes are sets: every member is a key that is deleted, as is the set
	// itself.
	Indexes []string
	Keys    []string
	Markers []string
}

// SetMember is a key and a member, to check membership of.
type SetMember struct {
	Key    string
	Member string
}

// CacheMutations are the writes a cache supports.
type CacheMutations interface {
	// AddMembersToSet adds members to a set, each with its own time to live.
	// The set itself lives as long as its longest-lived member.
	AddMembersToSet(ctx context.Context, key string, members []Member, options AddMembersOptions) error
	Clear(ctx context.Context, keys []string) error
	RemoveMembersFromSet(ctx context.Context, key string, members []string) error
	Set(ctx context.Context, key string, data string, ttl time.Duration) error
}

// Cache is the storage behind the lazy invalidation strategy.
type Cache interface {
	CacheMutations
	Exists(ctx context.Context, key string) (bool, error)
	// Get returns a value, and whether it exists.
	Get(ctx context.Context, key string) (string, bool, error)
	// GetEpoch returns the current invalidation epoch.
	GetEpoch(ctx context.Context) (int64, error)
	// GetWithEpoch reads the current invalidation epoch, then a value.
	GetWithEpoch(ctx context.Context, key string) (epoch int64, value string, found bool, err error)
	// GetPipe returns a pipe: mutations queued on it are applied atomically
	// by Execute, and only if the optional condition holds.
	GetPipe(condition *StoreCondition) (CachePipe, error)
	GetSetMembers(ctx context.Context, key string) ([]string, error)
	// Invalidate atomically bumps the invalidation epoch, records it for
	// every marker and deletes the given keys (and the keys listed in the
	// given index sets).
	Invalidate(ctx context.Context, params InvalidateParams) error
	IsSetMember(ctx context.Context, key, member string) (bool, error)
	// AreSetMembers checks several memberships at once.
	AreSetMembers(ctx context.Context, members []SetMember) ([]bool, error)
}

// CachePipe queues mutations and applies them atomically.
type CachePipe interface {
	CacheMutations
	Execute(ctx context.Context) error
}
