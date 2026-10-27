package testserver

import "sync"

// Store is the data behind the test server.
type Store struct {
	mu     sync.Mutex
	items  []*Item
	owners map[string]*Owner
	calls  map[string]int
	// Secret, if set, is returned as every item's secret field, but only to
	// requests that are allowed to see it (see the server's field
	// middleware in the tests).
	Secret string
}

// NewStore returns a store with two items and two owners.
func NewStore() *Store {
	return &Store{
		items: []*Item{
			{ID: "1", Name: "one", Owner: &Owner{ID: "o1"}},
			{ID: "2", Name: "two", Owner: &Owner{ID: "o2"}},
		},
		owners: map[string]*Owner{
			"o1": {ID: "o1", Name: "Ann"},
			"o2": {ID: "o2", Name: "Bob"},
		},
		calls: map[string]int{},
	}
}

// Calls returns how often a root field was resolved.
func (s *Store) Calls(field string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[field]
}

func (s *Store) count(field string) {
	s.calls[field]++
}

// SetOwnerName renames an owner.
func (s *Store) SetOwnerName(id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners[id].Name = name
}

// SetItemName renames an item.
func (s *Store) SetItemName(id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.items {
		if item.ID == id {
			item.Name = name
		}
	}
}

// AddItem adds an item.
func (s *Store) AddItem(id, name, ownerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, &Item{ID: id, Name: name, Owner: &Owner{ID: ownerID}})
}

func (s *Store) copyItem(item *Item) *Item {
	copied := *item
	copied.Owner = &Owner{ID: item.Owner.ID}
	return &copied
}

func (s *Store) copyOwner(id string) *Owner {
	owner, ok := s.owners[id]
	if !ok {
		return nil
	}
	copied := *owner
	return &copied
}
