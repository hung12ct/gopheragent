package memory

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// NewInMemStore returns an in-process Store backed by a map. Use for tests
// and single-node deployments where rebuilding memory on restart is
// acceptable (the consolidator will rebuild within a few sessions).
func NewInMemStore() *InMemStore {
	return &InMemStore{notes: make(map[string]map[string]Note)}
}

// InMemStore is a process-local Store. Safe for concurrent use; reads
// take an RLock so the loader hot-path scales across goroutines.
type InMemStore struct {
	mu    sync.RWMutex
	notes map[string]map[string]Note // scope -> key -> note
}

// Put inserts or updates a note. CreatedAt is preserved across updates.
func (s *InMemStore) Put(_ context.Context, scope string, note Note) error {
	if note.Key == "" {
		return fmt.Errorf("memory: note key is required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.notes[scope]
	if !ok {
		bucket = make(map[string]Note)
		s.notes[scope] = bucket
	}
	if existing, found := bucket[note.Key]; found {
		note.CreatedAt = existing.CreatedAt
	} else if note.CreatedAt.IsZero() {
		note.CreatedAt = now
	}
	note.UpdatedAt = now
	bucket[note.Key] = note
	return nil
}

// List returns every note under scope, ordered by UpdatedAt descending.
func (s *InMemStore) List(_ context.Context, scope string) ([]Note, error) {
	s.mu.RLock()
	bucket, ok := s.notes[scope]
	if !ok {
		s.mu.RUnlock()
		return nil, nil
	}
	out := make([]Note, 0, len(bucket))
	for _, n := range bucket {
		out = append(out, n)
	}
	s.mu.RUnlock()
	slices.SortFunc(out, func(a, b Note) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})
	return out, nil
}

// Delete removes one note. No-op when missing.
func (s *InMemStore) Delete(_ context.Context, scope, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bucket, ok := s.notes[scope]; ok {
		delete(bucket, key)
		if len(bucket) == 0 {
			delete(s.notes, scope)
		}
	}
	return nil
}
