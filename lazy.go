package mangrove

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// LazyStrategyOptions configure the lazy invalidation strategy.
type LazyStrategyOptions struct {
	Cache             Cache
	BuildEntityKey    func(EntityRecord) string
	BuildOperationKey func(cacheKey string) string
	BuildResultKey    func(cacheKey string) string
	BuildTypeIndexKey func(typename string) string
	Logger            *slog.Logger
	// Now is the clock, for tests. Defaults to time.Now.
	Now func() time.Time
}

// lazyCacheState is the state carried from reading the cache to storing the
// result.
type lazyCacheState struct {
	// epoch is the invalidation epoch observed before anything was read.
	epoch int64
	// readAt is when the request started reading (ms). Data fetched by the
	// request is at most this old, so expiries are computed from it.
	readAt int64
	// expiresAt is the expiry (ms) of the cached result this request started
	// from, or 0.
	expiresAt int64
	// validEntityKeys are the entities that were valid when the cached result
	// was read, and whose data therefore wasn't refetched.
	validEntityKeys []string
	// version of the cached result this request started from, or "".
	version string
}

type storedEnvelope struct {
	V         int    `json:"v"`
	ExpiresAt int64  `json:"expiresAt"`
	Version   string `json:"version"`
	// Opaque are the coordinates holding objects that aren't identifiable
	// entities.
	Opaque []string     `json:"opaque"`
	Result storedResult `json:"result"`
}

type storedResult struct {
	Data   json.RawMessage `json:"data"`
	Errors gqlerror.List   `json:"errors,omitempty"`
}

type storedReference struct {
	// I is the entity id, or nil for entities identified by typename alone.
	I *string    `json:"i"`
	P []PathPart `json:"p"`
	T string     `json:"t"`
}

func parseEnvelope(serialized string) (*storedEnvelope, *CachedResult, error) {
	var raw struct {
		V         *int            `json:"v"`
		ExpiresAt *int64          `json:"expiresAt"`
		Version   *string         `json:"version"`
		Opaque    []string        `json:"opaque"`
		Result    json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(serialized), &raw); err != nil {
		return nil, nil, err
	}
	if raw.V == nil || *raw.V != 2 || raw.ExpiresAt == nil || raw.Version == nil || raw.Opaque == nil || len(raw.Result) == 0 || raw.Result[0] != '{' {
		return nil, nil, nil
	}
	var result storedResult
	if err := json.Unmarshal(raw.Result, &result); err != nil {
		return nil, nil, err
	}
	cached := &CachedResult{Errors: result.Errors}
	if len(result.Data) > 0 {
		data, err := DecodeJSON(result.Data)
		if err != nil {
			return nil, nil, err
		}
		cached.Data = asObject(data)
	}
	return &storedEnvelope{
		V:         2,
		ExpiresAt: *raw.ExpiresAt,
		Version:   *raw.Version,
		Opaque:    raw.Opaque,
	}, cached, nil
}

const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

func createVersion(nowMs int64) string {
	random := rand.Uint64()
	suffix := make([]byte, 0, 13)
	for i := 0; i < 11; i++ {
		suffix = append(suffix, base36[random%36])
		random /= 36
	}
	return strconv.FormatInt(nowMs, 36) + "." + string(suffix)
}

// versionMember is the member of an entity's set vouching for one version of
// a result.
func versionMember(cacheKey, version string) string {
	return cacheKey + "@" + version
}

func serializeReference(entity EntityRecord, path []PathPart) (string, error) {
	encoded, err := json.Marshal(storedReference{I: entity.ID, P: path, T: entity.Typename})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func parseReference(serialized string) (entityCacheResult, error) {
	var reference struct {
		I *string    `json:"i"`
		P []PathPart `json:"p"`
		T *string    `json:"t"`
	}
	if err := json.Unmarshal([]byte(serialized), &reference); err != nil {
		return entityCacheResult{}, err
	}
	if reference.T == nil || len(reference.P) == 0 {
		return entityCacheResult{}, fmt.Errorf("invalid entity reference %s", serialized)
	}
	return entityCacheResult{
		EntityWithLocation: EntityWithLocation{
			Entity: EntityRecord{Typename: *reference.T, ID: reference.I},
			Path:   reference.P,
		},
	}, nil
}

// LazyStrategy keeps the adaptive logic in the execution path: invalidating
// an entity simply deletes its key, and each request works out what to
// refetch by checking which of the entities in its cached result are still
// valid.
//
// Every entity has a set of the cached operations that hold valid data for
// it. A cached result stays valid for an entity as long as the operation is a
// member of that entity's set.
type LazyStrategy struct {
	cache             Cache
	buildEntityKey    func(EntityRecord) string
	buildOperationKey func(string) string
	buildResultKey    func(string) string
	buildTypeIndexKey func(string) string
	logger            *slog.Logger
	now               func() time.Time
}

// NewLazyStrategy creates the lazy invalidation strategy.
func NewLazyStrategy(options LazyStrategyOptions) *LazyStrategy {
	s := &LazyStrategy{
		cache:             options.Cache,
		buildEntityKey:    options.BuildEntityKey,
		buildOperationKey: options.BuildOperationKey,
		buildResultKey:    options.BuildResultKey,
		buildTypeIndexKey: options.BuildTypeIndexKey,
		logger:            options.Logger,
		now:               options.Now,
	}
	if s.buildEntityKey == nil {
		s.buildEntityKey = DefaultBuildEntityKey
	}
	if s.buildOperationKey == nil {
		s.buildOperationKey = DefaultBuildOperationKey
	}
	if s.buildResultKey == nil {
		s.buildResultKey = DefaultBuildResultKey
	}
	if s.buildTypeIndexKey == nil {
		s.buildTypeIndexKey = DefaultBuildTypeIndexKey
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

func (s *LazyStrategy) nowMs() int64 {
	return s.now().UnixMilli()
}

// GetPartialExecutionOpts reads the cached result of a request and works out
// what needs to be refetched.
func (s *LazyStrategy) GetPartialExecutionOpts(ctx context.Context, params GetPartialExecutionOptsParams) (*PartialExecutionOpts, error) {
	readAt := s.nowMs()
	// The epoch must be read before anything else, so that invalidations
	// that happen while this request runs are detected when storing.
	epoch, serialized, found, err := s.cache.GetWithEpoch(ctx, s.buildResultKey(params.CacheKey))
	if err != nil {
		return nil, err
	}

	uncached := &PartialExecutionOpts{
		CacheState:     &lazyCacheState{epoch: epoch, readAt: readAt},
		LinkSelections: map[string]ast.SelectionSet{},
		Query:          params.Query,
	}

	if !found {
		return uncached, nil
	}

	envelope, cachedResult, references, err := func() (*storedEnvelope, *CachedResult, []entityCacheResult, error) {
		envelope, cachedResult, err := parseEnvelope(serialized)
		if err != nil {
			return nil, nil, nil, err
		}

		if envelope == nil || envelope.ExpiresAt <= s.nowMs() {
			return nil, nil, nil, nil
		}

		serializedReferences, err := s.cache.GetSetMembers(ctx, s.buildOperationKey(params.CacheKey))
		if err != nil {
			return nil, nil, nil, err
		}

		references := make([]entityCacheResult, 0, len(serializedReferences))
		for _, serializedReference := range serializedReferences {
			reference, err := parseReference(serializedReference)
			if err != nil {
				return nil, nil, nil, err
			}
			references = append(references, reference)
		}
		return envelope, cachedResult, references, nil
	}()
	if err != nil {
		s.logger.ErrorContext(ctx, "Error reading cached result, falling back to uncached execution", "error", err)
		return uncached, nil
	}

	if envelope == nil || len(references) == 0 {
		return uncached, nil
	}

	var entityKeys []string
	seen := map[string]bool{}
	for _, reference := range references {
		key := s.buildEntityKey(reference.Entity)
		if !seen[key] {
			seen[key] = true
			entityKeys = append(entityKeys, key)
		}
	}

	member := versionMember(params.CacheKey, envelope.Version)
	queries := make([]SetMember, len(entityKeys))
	for i, key := range entityKeys {
		queries[i] = SetMember{Key: key, Member: member}
	}
	memberships, err := s.cache.AreSetMembers(ctx, queries)
	if err != nil {
		return nil, err
	}

	validity := make(map[string]bool, len(entityKeys))
	validEntityKeys := []string{}
	for i, key := range entityKeys {
		validity[key] = memberships[i]
		if memberships[i] {
			validEntityKeys = append(validEntityKeys, key)
		}
	}

	for i := range references {
		references[i].invalidated = !validity[s.buildEntityKey(references[i].Entity)]
	}

	cacheState := &lazyCacheState{
		epoch:           epoch,
		readAt:          readAt,
		expiresAt:       envelope.ExpiresAt,
		validEntityKeys: validEntityKeys,
		version:         envelope.Version,
	}

	partial, err := func() (*partialRecacheQuery, error) {
		tree, err := buildEntityTree(references, params.Resolvers, envelope.Opaque)
		if err != nil {
			return nil, err
		}
		return getPartialRecacheQuery(params.Query, tree, params.OperationName)
	}()
	if err != nil {
		s.logger.ErrorContext(ctx, "Unexpected error when building partial query, falling back to uncached execution", "error", err)
		return uncached, nil
	}

	opts := &PartialExecutionOpts{
		CacheState:     cacheState,
		LinkSelections: map[string]ast.SelectionSet{},
		Result:         cachedResult,
	}
	if partial != nil {
		opts.LinkSelections = partial.linkSelections
		opts.PrunedCoordinates = partial.prunedCoordinates
		opts.Query = partial.query
		opts.Resolutions = partial.resolutions
	}
	return opts, nil
}

// InvalidateEntities invalidates entities. An entity without an id
// invalidates every entity of its type, and the type itself.
func (s *LazyStrategy) InvalidateEntities(ctx context.Context, entities []EntityRecord) error {
	var keys, indexes []string
	seenKeys := map[string]bool{}
	seenIndexes := map[string]bool{}

	addKey := func(key string) {
		if !seenKeys[key] {
			seenKeys[key] = true
			keys = append(keys, key)
		}
	}

	for _, entity := range entities {
		if entity.Typename == "" {
			return errors.New("can't invalidate an entity without a typename")
		}

		if entity.ID != nil {
			addKey(s.buildEntityKey(entity))
		} else {
			// Every entity of the type, and the type itself.
			addKey(s.buildEntityKey(EntityRecord{Typename: entity.Typename}))
			index := s.buildTypeIndexKey(entity.Typename)
			if !seenIndexes[index] {
				seenIndexes[index] = true
				indexes = append(indexes, index)
			}
		}
	}

	if len(keys) == 0 {
		return nil
	}

	return s.cache.Invalidate(ctx, InvalidateParams{Indexes: indexes, Keys: keys, Markers: keys})
}

func ms(d time.Duration) int64 {
	return d.Milliseconds()
}

func durationMs(ms int64) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

// StoreExecutionResult stores a result.
func (s *LazyStrategy) StoreExecutionResult(ctx context.Context, params StoreParams) error {
	state, ok := params.CacheState.(*lazyCacheState)
	if !ok || state == nil {
		s.logger.WarnContext(ctx, "Not storing result: it was not produced from a cache read by this strategy")
		return nil
	}

	now := s.nowMs()
	ttl := ms(params.TTL)
	// Everything fetched by this request is at most as old as the request.
	age := max(0, now-state.readAt)

	// A merged result is only as fresh as the cached result it was merged
	// into, so it keeps that result's expiry.
	expiresAt := state.readAt + ttl
	if params.MergedIntoCachedResult && state.expiresAt != 0 {
		expiresAt = state.expiresAt
	}

	// Clamped, in case the clock jumped backwards during the request.
	remaining := min(expiresAt-now, ttl)

	if remaining <= 0 {
		return nil
	}

	version := createVersion(now)
	member := versionMember(params.CacheKey, version)

	var data any
	if params.Result.Data != nil {
		data = params.Result.Data
	}
	encodedData, err := EncodeJSON(data)
	if err != nil {
		return err
	}
	envelope, err := json.Marshal(storedEnvelope{
		V:         2,
		ExpiresAt: expiresAt,
		Version:   version,
		Opaque:    collectOpaqueCoordinates(data),
		Result:    storedResult{Data: encodedData, Errors: params.Result.Errors},
	})
	if err != nil {
		return err
	}

	type entityEntry struct {
		entity    EntityRecord
		memberTTL int64
	}
	entities := map[string]*entityEntry{}
	var entityOrder []string
	var references []string
	seenReferences := map[string]bool{}

	for _, collected := range params.CollectedEntities {
		entityKey := s.buildEntityKey(collected.Entity)
		entityTTL := ttl
		if typeTTL, ok := params.EntityTTLs[collected.Entity.Typename]; ok {
			entityTTL = ms(typeTTL)
		}
		entityTTL = min(entityTTL-age, remaining)

		if existing, ok := entities[entityKey]; !ok {
			entities[entityKey] = &entityEntry{entity: collected.Entity, memberTTL: entityTTL}
			entityOrder = append(entityOrder, entityKey)
		} else if existing.memberTTL > entityTTL {
			existing.entity = collected.Entity
			existing.memberTTL = entityTTL
		}

		reference, err := serializeReference(collected.Entity, collected.Path)
		if err != nil {
			return err
		}
		if !seenReferences[reference] {
			seenReferences[reference] = true
			references = append(references, reference)
		}
	}

	operationKey := s.buildOperationKey(params.CacheKey)

	// Data of entities that were still valid in the cached result was taken
	// from the cache, not refetched. Their validity is carried over from the
	// version that was read, if (and for as long as) it still holds.
	inheritedFrom := ""
	notRefetched := map[string]bool{}
	if params.MergedIntoCachedResult && state.version != "" {
		inheritedFrom = versionMember(params.CacheKey, state.version)
		for _, key := range state.validEntityKeys {
			notRefetched[key] = true
		}
	}

	// Never overwrite a result stored by a request that started later.
	pipe, err := s.cache.GetPipe(&StoreCondition{
		Epoch: state.epoch,
		Key:   operationKey + ":epoch",
		TTL:   durationMs(remaining),
	})
	if err != nil {
		return err
	}

	for _, entityKey := range entityOrder {
		entry := entities[entityKey]
		if entry.memberTTL <= 0 {
			continue
		}

		typeKey := s.buildEntityKey(EntityRecord{Typename: entry.entity.Typename})
		markers := []string{entityKey, typeKey}
		if entityKey == typeKey {
			markers = []string{entityKey}
		}
		options := AddMembersOptions{Guard: &InvalidationGuard{Epoch: state.epoch, Markers: markers}}
		if notRefetched[entityKey] {
			options.InheritFrom = inheritedFrom
		}

		if err := pipe.AddMembersToSet(ctx, entityKey, []Member{{Member: member, TTL: durationMs(entry.memberTTL)}}, options); err != nil {
			return err
		}

		if entry.entity.ID != nil {
			if err := pipe.AddMembersToSet(ctx, s.buildTypeIndexKey(entry.entity.Typename), []Member{{Member: entityKey, TTL: durationMs(entry.memberTTL)}}, AddMembersOptions{}); err != nil {
				return err
			}
		}
	}

	if err := pipe.Clear(ctx, []string{operationKey}); err != nil {
		return err
	}
	referenceMembers := make([]Member, len(references))
	for i, reference := range references {
		referenceMembers[i] = Member{Member: reference, TTL: durationMs(remaining)}
	}
	if err := pipe.AddMembersToSet(ctx, operationKey, referenceMembers, AddMembersOptions{}); err != nil {
		return err
	}
	if err := pipe.Set(ctx, s.buildResultKey(params.CacheKey), string(envelope), durationMs(remaining)); err != nil {
		return err
	}

	return pipe.Execute(ctx)
}
