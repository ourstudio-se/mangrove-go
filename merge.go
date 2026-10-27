package mangrove

import (
	"fmt"
	"sort"
	"strings"
)

// CacheMergeError is raised when a partial result can't be merged into the
// cached result in a way that is guaranteed to be correct. The runner reacts
// by executing the original document in full.
type CacheMergeError struct {
	Message string
}

func (e *CacheMergeError) Error() string { return e.Message }

func identityOf(typename, id string) string {
	return typename + "\x00" + id
}

func getEntityIdentity(value any) (string, bool) {
	object, ok := value.(*Object)
	if !ok || object == nil {
		return "", false
	}
	typename, ok := getMarkedTypename(object)
	if !ok {
		return "", false
	}
	id, ok := getMarkedID(object)
	if !ok {
		return "", false
	}
	return identityOf(typename, id), true
}

// isLinkStub reports whether value is an object that only identifies an
// entity, left behind by a link.
func isLinkStub(value any) bool {
	object, ok := value.(*Object)
	if !ok || object == nil {
		return false
	}
	if _, ok := getEntityIdentity(object); !ok {
		return false
	}
	for _, f := range object.fields {
		if !isMarkerKey(f.key) {
			return false
		}
	}
	return true
}

type entitiesByCoordinates map[string]map[string]*Object

func (e entitiesByCoordinates) get(coordinates, identity string) *Object {
	return e[coordinates][identity]
}

func (e entitiesByCoordinates) set(coordinates, identity string, entity *Object) {
	entities := e[coordinates]
	if entities == nil {
		entities = map[string]*Object{}
		e[coordinates] = entities
	}
	entities[identity] = entity
}

// entitySnapshots holds the entities found in a (cached) result, by the
// coordinates they were found at. The selection set at a coordinate is the
// same for every object found there, which makes a snapshot from one
// position a valid stand-in for another.
type entitySnapshots struct {
	byCoordinates entitiesByCoordinates
}

func newEntitySnapshots(data *Object) *entitySnapshots {
	s := &entitySnapshots{byCoordinates: entitiesByCoordinates{}}
	if data != nil {
		s.index(data, rootQuery)
	}
	return s
}

func (s *entitySnapshots) get(coordinates, identity string) *Object {
	return s.byCoordinates.get(coordinates, identity)
}

func (s *entitySnapshots) has(coordinates, identity string) bool {
	_, ok := s.byCoordinates[coordinates][identity]
	return ok
}

// set records fresher data for an entity, so that later stubs for it (e.g.
// in subsequent link query rounds) are completed with up-to-date data.
func (s *entitySnapshots) set(coordinates, identity string, entity *Object) {
	s.byCoordinates.set(coordinates, identity, entity)
}

func (s *entitySnapshots) index(data any, coordinates string) {
	switch v := data.(type) {
	case []any:
		for _, item := range v {
			s.index(item, coordinates)
		}
	case *Object:
		if v == nil {
			return
		}
		// Cached results never contain unresolved links, so even entities
		// without any selected fields are genuine data.
		if identity, ok := getEntityIdentity(v); ok && !s.has(coordinates, identity) {
			s.set(coordinates, identity, v)
		}
		for _, f := range v.fields {
			s.index(f.value, coordinates+"."+f.key)
		}
	}
}

// freshEntities holds the fresh data received for entities during the
// current request, in the order it was received. Copies of cached snapshots
// are patched with it, so that data from the cache never resurrects
// something that was just refetched.
type freshEntities struct {
	byCoordinates entitiesByCoordinates
}

func (f *freshEntities) add(coordinates, identity string, entity *Object) {
	// Everything received for the same entity at the same coordinates comes
	// from the same selection set (possibly narrowed): overlay it.
	f.byCoordinates.set(coordinates, identity, overlay(f.byCoordinates.get(coordinates, identity), entity))
}

func (f *freshEntities) get(coordinates, identity string) *Object {
	return f.byCoordinates.get(coordinates, identity)
}

// overlay overlays source onto a copy of target, field by field.
func overlay(target, source *Object) *Object {
	result := shallowCopy(target)
	for _, f := range source.fields {
		existing := result.value(f.key)
		if sourceObject, ok := f.value.(*Object); ok && sourceObject != nil && isObject(existing) {
			result.Set(f.key, overlay(existing.(*Object), sourceObject))
		} else {
			result.Set(f.key, f.value)
		}
	}
	return result
}

type mergeContext struct {
	fresh *freshEntities
	// linkCoordinates are the coordinates at which the generated queries
	// select links.
	linkCoordinates map[string]bool
	// prunedCoordinates are the coordinates whose selection sets were
	// narrowed in the partial query.
	prunedCoordinates map[string]bool
	snapshots         *entitySnapshots
}

func newMergeContext(cachedData *Object, prunedCoordinates []string, linkCoordinates []string) *mergeContext {
	context := &mergeContext{
		fresh:             &freshEntities{byCoordinates: entitiesByCoordinates{}},
		linkCoordinates:   map[string]bool{},
		prunedCoordinates: map[string]bool{},
		snapshots:         newEntitySnapshots(cachedData),
	}
	for _, coordinates := range prunedCoordinates {
		context.prunedCoordinates[coordinates] = true
	}
	for _, coordinates := range linkCoordinates {
		context.linkCoordinates[coordinates] = true
	}
	return context
}

// patchWithFresh applies the fresh data received so far to (a copy of)
// cached data.
func patchWithFresh(value any, coordinates string, context *mergeContext) (any, error) {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			patched, err := patchWithFresh(item, coordinates, context)
			if err != nil {
				return nil, err
			}
			out[i] = patched
		}
		return out, nil
	case *Object:
		if v == nil {
			return v, nil
		}
		patched := v

		if identity, ok := getEntityIdentity(patched); ok {
			if fresh := context.fresh.get(coordinates, identity); fresh != nil {
				merged, err := mergeData(patched, fresh, coordinates, context, false)
				if err != nil {
					return nil, err
				}
				patched = merged.(*Object)
			}
		}

		for i := range patched.fields {
			child, err := patchWithFresh(patched.fields[i].value, coordinates+"."+patched.fields[i].key, context)
			if err != nil {
				return nil, err
			}
			patched.fields[i].value = child
		}

		return patched, nil
	default:
		return value, nil
	}
}

// recordFresh records every entity in freshly fetched data as fresh,
// whether or not it ends up being merged anywhere.
func recordFresh(value any, coordinates string, context *mergeContext) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			recordFresh(item, coordinates, context)
		}
	case *Object:
		if v == nil {
			return
		}
		if identity, ok := getEntityIdentity(v); ok && !isLinkStub(v) {
			context.fresh.add(coordinates, identity, v)
		}
		for _, f := range v.fields {
			recordFresh(f.value, coordinates+"."+f.key, context)
		}
	}
}

// completeLink returns the data to complete a link stub with: a cached
// snapshot of the entity at the same coordinates (patched with fresh data),
// or else fresh data received for it at those coordinates earlier in the
// request.
func completeLink(coordinates, identity string, context *mergeContext) (*Object, error) {
	if snapshot := context.snapshots.get(coordinates, identity); snapshot != nil {
		return copySnapshot(snapshot, coordinates, context)
	}
	if fresh := context.fresh.get(coordinates, identity); fresh != nil {
		return cloneObject(fresh), nil
	}
	return nil, nil
}

// copySnapshot returns a private, up-to-date copy of a cached snapshot.
func copySnapshot(snapshot *Object, coordinates string, context *mergeContext) (*Object, error) {
	patched, err := patchWithFresh(cloneObject(snapshot), coordinates, context)
	if err != nil {
		return nil, err
	}
	return patched.(*Object), nil
}

func sameShape(target, source *Object) bool {
	if sourceIdentity, ok := getEntityIdentity(source); ok {
		targetIdentity, ok := getEntityIdentity(target)
		return ok && targetIdentity == sourceIdentity
	}
	sourceType, sourceOK := getMarkedTypename(source)
	targetType, targetOK := getMarkedTypename(target)
	if sourceOK && targetOK {
		return sourceType == targetType
	}
	return true
}

func allMarkerKeys(object *Object) bool {
	for _, f := range object.fields {
		if !isMarkerKey(f.key) {
			return false
		}
	}
	return true
}

// mergeData merges freshly fetched data (source) into previously cached data
// (target) found at coordinates, returning the merged value. target may be
// mutated, and must not be shared with anything else.
//
//   - Objects are merged field by field, but only if they are the same
//     entity (or the same type, for objects without an id); otherwise the
//     source replaces the target.
//   - A source object for an entity that isn't at the same position in the
//     target is merged into a snapshot of that entity from elsewhere in the
//     cached result at the same coordinates, if there is one.
//   - Lists are matched entity by entity; non-entity items by index.
func mergeData(target, source any, coordinates string, context *mergeContext, record bool) (any, error) {
	snapshots := context.snapshots

	switch s := source.(type) {
	case []any:
		targetList, _ := target.([]any)
		var byIdentity map[string]any

		out := make([]any, len(s))
		for index, item := range s {
			identity, ok := getEntityIdentity(item)

			if !ok {
				var targetItem any
				if index < len(targetList) {
					targetItem = targetList[index]
				}
				merged, err := mergeData(targetItem, item, coordinates, context, record)
				if err != nil {
					return nil, err
				}
				out[index] = merged
				continue
			}

			if byIdentity == nil {
				byIdentity = map[string]any{}
				for _, candidate := range targetList {
					if candidateIdentity, ok := getEntityIdentity(candidate); ok {
						if _, exists := byIdentity[candidateIdentity]; !exists {
							byIdentity[candidateIdentity] = candidate
						}
					}
				}
			}

			var base any
			if inList, ok := byIdentity[identity]; ok {
				base = cloneData(inList)
			} else if isLinkStub(item) {
				completed, err := completeLink(coordinates, identity, context)
				if err != nil {
					return nil, err
				}
				if completed != nil {
					base = completed
				}
			}
			merged, err := mergeData(base, item, coordinates, context, record)
			if err != nil {
				return nil, err
			}
			out[index] = merged
		}
		return out, nil

	case *Object:
		if s == nil {
			return nil, nil
		}

		if _, hasIdentity := getEntityIdentity(s); context.linkCoordinates[coordinates] && !hasIdentity && allMarkerKeys(s) {
			// A link to something that isn't an identifiable entity: its data
			// was stripped from the query and can't be fetched through a link.
			return nil, &CacheMergeError{Message: fmt.Sprintf("Object at %s can't be resolved through a link", coordinates)}
		}

		var base *Object
		if targetObject, ok := target.(*Object); ok && targetObject != nil && sameShape(targetObject, s) {
			base = targetObject
		}

		if base == nil {
			if context.prunedCoordinates[coordinates] && !isLinkStub(s) {
				// Only part of this object was fetched, on the assumption that
				// it was still the same object as in the cached result. It
				// isn't, so the cached data can't be trusted to complete it.
				return nil, &CacheMergeError{Message: fmt.Sprintf("Object at %s changed without being invalidated", coordinates)}
			}
			// Only links need completing; anything else was fetched in full.
			if isLinkStub(s) {
				identity, _ := getEntityIdentity(s)
				completed, err := completeLink(coordinates, identity, context)
				if err != nil {
					return nil, err
				}
				base = completed
			}
			if base == nil {
				base = NewObject(s.Len())
			}
		}

		for _, f := range s.fields {
			merged, err := mergeData(base.value(f.key), f.value, coordinates+"."+f.key, context, record)
			if err != nil {
				return nil, err
			}
			base.Set(f.key, merged)
		}

		if identity, ok := getEntityIdentity(s); ok && !isLinkStub(s) {
			snapshots.set(coordinates, identity, base)
			if record {
				context.fresh.add(coordinates, identity, s)
			}
		}

		return base, nil

	default:
		return source, nil
	}
}

// mergeResolvedEntities merges entities fetched through a cache resolver
// into every position at coordinates (absolute, `Query.…`) where an entity
// with the same identity is found.
func mergeResolvedEntities(data *Object, coordinates string, resolved any, context *mergeContext, expected *Expectation) error {
	fetched := map[string]*Object{}

	items, isList := resolved.([]any)
	if !isList {
		items = []any{resolved}
	}

	for _, entity := range items {
		identity, ok := getEntityIdentity(entity)
		if !ok {
			// A resolver returned null (or something without an identity)
			// for an entity that we know exists in the cached result.
			return &CacheMergeError{Message: fmt.Sprintf("Cache resolver at %s did not return a resolvable entity", coordinates)}
		}
		fetched[identity] = entity.(*Object)
		recordFresh(entity, coordinates, context)
	}

	if expected != nil {
		for _, id := range expected.IDs {
			if _, ok := fetched[identityOf(expected.Typename, id)]; !ok {
				return &CacheMergeError{Message: fmt.Sprintf("Cache resolver at %s did not return %s:%s", coordinates, expected.Typename, id)}
			}
		}
	}

	parts := strings.Split(coordinates, ".")
	root, fields := parts[0], parts[1:]

	if root != rootQuery || len(fields) == 0 {
		return &CacheMergeError{Message: fmt.Sprintf("Invalid resolver coordinates %s", coordinates)}
	}

	var visit func(container any, depth int) (any, error)
	visit = func(container any, depth int) (any, error) {
		if list, ok := container.([]any); ok {
			out := make([]any, len(list))
			for i, item := range list {
				visited, err := visit(item, depth)
				if err != nil {
					return nil, err
				}
				out[i] = visited
			}
			return out, nil
		}

		if depth == len(fields) {
			identity, ok := getEntityIdentity(container)
			if !ok {
				return container, nil
			}
			entity := fetched[identity]
			if entity == nil {
				return container, nil
			}
			return mergeData(container, entity, coordinates, context, false)
		}

		if object, ok := container.(*Object); ok && object != nil {
			field := fields[depth]
			if i := object.index(field); i >= 0 {
				visited, err := visit(object.fields[i].value, depth+1)
				if err != nil {
					return nil, err
				}
				object.fields[i].value = visited
			}
		}

		return container, nil
	}

	// Entities that are no longer at these coordinates (e.g. removed from a
	// refetched list) are simply not merged anywhere.
	_, err := visit(data, 0)
	return err
}

// mergeExecutionData merges one execution result (partial query or link
// query) into data. Root fields aliased as cache resolutions are merged into
// their encoded coordinates; everything else is merged in place.
func mergeExecutionData(data, nextData *Object, context *mergeContext, expectations ResolutionExpectations) (*Object, error) {
	plain := NewObject(nextData.Len())

	type resolution struct {
		coordinates string
		value       any
		alias       string
		depth       int
	}
	var resolutions []resolution

	for _, f := range nextData.fields {
		if isCacheAliasName(f.key) {
			coordinates, err := parseCacheResolverAlias(f.key)
			if err != nil {
				return nil, err
			}
			resolutions = append(resolutions, resolution{
				coordinates: coordinates,
				value:       f.value,
				alias:       f.key,
				depth:       strings.Count(coordinates, ".") + 1,
			})
		} else {
			plain.Set(f.key, f.value)
		}
	}

	merged, err := mergeData(data, plain, rootQuery, context, true)
	if err != nil {
		return nil, err
	}
	mergedObject := merged.(*Object)

	// Shallower coordinates first: merging them can create the positions that
	// deeper resolutions are merged into.
	sort.SliceStable(resolutions, func(i, j int) bool {
		return resolutions[i].depth < resolutions[j].depth
	})

	for _, r := range resolutions {
		var expected *Expectation
		if expectation, ok := expectations[r.alias]; ok {
			expected = &expectation
		}
		if err := mergeResolvedEntities(mergedObject, r.coordinates, r.value, context, expected); err != nil {
			return nil, err
		}
	}

	return mergedObject, nil
}
