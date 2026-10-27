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
