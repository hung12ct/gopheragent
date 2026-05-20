package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// InMemStoreOpt configures an InMemStore at construction.
type InMemStoreOpt func(*InMemStore)

// DefaultMaxNotesPerScope is the cap applied by NewInMemStore when the
// caller doesn't pass WithMaxNotesPerScope. Picked to be permissive but
// not unbounded: 100 notes × ~30 tokens ≈ 3K tokens — well above what
// the loader's default TokenBudget will surface, low enough that an
// adopter that forgets to wire a Consolidator still can't blow out
// memory by hand-Putting forever.
//
// Override (including disabling the cap with 0) via WithMaxNotesPerScope.
const DefaultMaxNotesPerScope = 100

// WithMaxNotesPerScope caps each scope's note count. When Put would
// exceed n, the least-recently-updated note in the scope is evicted
// before the new note lands — keeping the store bounded even when an
// upstream Consolidator drifts toward variable keys, or when an
// adopter calls Put directly without running a Consolidator. Pass 0
// to disable the cap entirely (unbounded — use only when you know
// growth is bounded elsewhere). Negative values are treated as 0.
//
// Pick n based on the loader's expected token budget. The default
// FormatNotes block is ~36 tokens of overhead plus ~21 tokens per
// note (one short sentence), so 100 notes ≈ 2100 tokens of system
// prompt before the loader's per-Run TokenBudget further trims it.
func WithMaxNotesPerScope(n int) InMemStoreOpt {
	return func(s *InMemStore) {
		if n < 0 {
			n = 0
		}
		s.maxPerScope = n
		s.capExplicit = true
	}
}

// NewInMemStore returns an in-process Store backed by a map. Use for
// tests and single-node deployments where rebuilding memory on restart
// is acceptable (the agent.Consolidator rebuilds within a few sessions).
//
// Bounded by default: callers who don't pass WithMaxNotesPerScope get
// DefaultMaxNotesPerScope as the cap. To disable the cap, pass
// WithMaxNotesPerScope(0) explicitly — opting in to unbounded growth
// is a deliberate choice the caller must make on the line.
func NewInMemStore(opts ...InMemStoreOpt) *InMemStore {
	s := &InMemStore{notes: make(map[string]map[string]Note)}
	for _, opt := range opts {
		opt(s)
	}
	if !s.capExplicit {
		s.maxPerScope = DefaultMaxNotesPerScope
	}
	return s
}

// InMemStore is a process-local Store. Safe for concurrent use; reads
// take an RLock so the loader hot-path scales across goroutines.
//
// Bounded: when maxPerScope > 0, Put evicts the least-recently-updated
// note in the scope before inserting a key that would overflow. The
// eviction is silent — callers see no error, just a smaller store than
// the naive insert count.
type InMemStore struct {
	mu          sync.RWMutex
	notes       map[string]map[string]Note // scope -> key -> note
	maxPerScope int                        // 0 = unlimited
	capExplicit bool                       // tracks WithMaxNotesPerScope override
}

// Put inserts or updates a note. CreatedAt is preserved across updates;
// UpdatedAt is stamped now. When the resulting scope size would exceed
// maxPerScope, the least-recently-updated note (other than the one
// being inserted) is evicted first.
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
	existing, found := bucket[note.Key]
	if found {
		note.CreatedAt = existing.CreatedAt
	} else if note.CreatedAt.IsZero() {
		note.CreatedAt = now
	}
	note.UpdatedAt = now

	// Eviction only kicks in for *new* keys that push us past the cap.
	// Updates of an existing key keep the scope size flat.
	if !found && s.maxPerScope > 0 && len(bucket) >= s.maxPerScope {
		evictOldest(bucket)
	}
	bucket[note.Key] = note
	return nil
}

// evictOldest removes the entry with the oldest UpdatedAt from bucket.
// Caller must hold s.mu. Ties on UpdatedAt break by Key ascending so
// the eviction order is deterministic even when rapid Puts land at the
// same monotonic-clock tick — without this, map iteration order would
// make tied evictions arbitrary. No-op on an empty bucket.
func evictOldest(bucket map[string]Note) {
	var oldestKey string
	var oldestAt time.Time
	first := true
	for k, n := range bucket {
		if first {
			oldestKey, oldestAt = k, n.UpdatedAt
			first = false
			continue
		}
		if n.UpdatedAt.Before(oldestAt) || (n.UpdatedAt.Equal(oldestAt) && k < oldestKey) {
			oldestKey, oldestAt = k, n.UpdatedAt
		}
	}
	if !first {
		delete(bucket, oldestKey)
	}
}

// List returns notes under scope filtered by opts and ordered by
// UpdatedAt descending. An unseen scope returns (nil, nil).
func (s *InMemStore) List(_ context.Context, scope string, opts ListOpts) ([]Note, error) {
	s.mu.RLock()
	bucket, ok := s.notes[scope]
	if !ok {
		s.mu.RUnlock()
		return nil, nil
	}
	out := make([]Note, 0, len(bucket))
	for _, n := range bucket {
		if !opts.UpdatedAfter.IsZero() && !n.UpdatedAt.After(opts.UpdatedAfter) {
			continue
		}
		if !matchesTags(n, opts.Tags) {
			continue
		}
		out = append(out, n)
	}
	s.mu.RUnlock()

	slices.SortFunc(out, func(a, b Note) int {
		// Newest first; deterministic tie-break by Key descending so
		// rapid same-nanosecond Puts produce a stable ordering. Without
		// this, two notes stamped at the same monotonic tick would
		// surface in random map-iteration order on each call.
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.Key, a.Key)
	})
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
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

// ReplaceAll atomically swaps every note under scope. CreatedAt is
// preserved from the prior state when a key matches; new keys get a
// fresh stamp. UpdatedAt is stamped on every entry. An empty notes
// slice clears the scope.
//
// Used by the Consolidator's merge path: one LLM call produces the
// curated full set, ReplaceAll publishes it without exposing a window
// where deletions and inserts are observable separately.
func (s *InMemStore) ReplaceAll(_ context.Context, scope string, notes []Note) error {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(notes) == 0 {
		delete(s.notes, scope)
		return nil
	}
	prior := s.notes[scope]
	next := make(map[string]Note, len(notes))
	for _, n := range notes {
		if n.Key == "" {
			return fmt.Errorf("memory: note key is required (ReplaceAll)")
		}
		if old, ok := prior[n.Key]; ok {
			n.CreatedAt = old.CreatedAt
		} else if n.CreatedAt.IsZero() {
			n.CreatedAt = now
		}
		n.UpdatedAt = now
		next[n.Key] = n
	}
	// Hard-cap apply: if maxPerScope set and the merged result is
	// over the cap, evict oldest until we fit. This handles the
	// Consolidator returning more than expected from a buggy prompt.
	for s.maxPerScope > 0 && len(next) > s.maxPerScope {
		evictOldest(next)
	}
	s.notes[scope] = next
	return nil
}
