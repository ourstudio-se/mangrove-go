package mangrove

import (
	"runtime"
	"sync"
	"weak"
)

// weakMemo memoizes a function of a pointer without keeping the pointer
// alive, like a WeakMap.
type weakMemo[K any, V any] struct {
	mu      sync.Mutex
	entries map[weak.Pointer[K]]V
}

func (m *weakMemo[K, V]) get(key *K, compute func(*K) V) V {
	pointer := weak.Make(key)

	m.mu.Lock()
	value, ok := m.entries[pointer]
	m.mu.Unlock()
	if ok {
		return value
	}

	value = compute(key)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[weak.Pointer[K]]V{}
	}
	if _, ok := m.entries[pointer]; !ok {
		m.entries[pointer] = value
		runtime.AddCleanup(key, func(pointer weak.Pointer[K]) {
			m.mu.Lock()
			delete(m.entries, pointer)
			m.mu.Unlock()
		}, pointer)
	}
	return value
}

// weakSet is a set of pointers that doesn't keep its members alive, like a
// WeakSet.
type weakSet[K any] struct {
	mu      sync.Mutex
	entries map[weak.Pointer[K]]struct{}
}

func (s *weakSet[K]) add(key *K) {
	pointer := weak.Make(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[weak.Pointer[K]]struct{}{}
	}
	if _, ok := s.entries[pointer]; ok {
		return
	}
	s.entries[pointer] = struct{}{}
	runtime.AddCleanup(key, func(pointer weak.Pointer[K]) {
		s.mu.Lock()
		delete(s.entries, pointer)
		s.mu.Unlock()
	}, pointer)
}

func (s *weakSet[K]) has(key *K) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[weak.Make(key)]
	return ok
}
