package history

import (
	"context"
	"sync"
	"testing"
)

// fakeMessageStore is an in-memory MessageStore for unit tests. The MySQL
// implementation must satisfy the same contract — keep these tests
// focused on the contract, not on storage mechanics.
type fakeMessageStore struct {
	mu       sync.Mutex
	rows     map[string][]Message
	saves    int // increments on every Save call
	saveLens []int
}

func newFakeMessageStore() *fakeMessageStore {
	return &fakeMessageStore{rows: make(map[string][]Message)}
}

func (f *fakeMessageStore) Save(_ context.Context, key string, msgs []Message, knownCount int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	f.saveLens = append(f.saveLens, len(msgs)-knownCount)

	cur := f.rows[key]
	if len(msgs) < knownCount {
		// Truncation
		cp := make([]Message, len(msgs))
		copy(cp, msgs)
		f.rows[key] = cp
		return len(msgs), nil
	}
	// Pure append OR upsert of full slice
	if knownCount == 0 || knownCount > len(cur) {
		cp := make([]Message, len(msgs))
		copy(cp, msgs)
		f.rows[key] = cp
		return len(msgs), nil
	}
	// Append starting at knownCount
	out := make([]Message, len(msgs))
	copy(out, msgs)
	f.rows[key] = out
	return len(msgs), nil
}

func (f *fakeMessageStore) Load(_ context.Context, key string) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.rows[key]
	cp := make([]Message, len(src))
	copy(cp, src)
	return cp, nil
}

func (f *fakeMessageStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, key)
	return nil
}

func TestMessageStore_Save_HintAdvancesAcrossCalls(t *testing.T) {
	store := newFakeMessageStore()
	msgs := []Message{
		{Role: "system", Content: "you are…"},
		{Role: "user", Content: "hi"},
	}
	count, err := store.Save(context.Background(), "s1", msgs, 0)
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if count != 2 {
		t.Fatalf("first save count: got %d, want 2", count)
	}

	msgs = append(msgs, Message{Role: "assistant", Content: "hello"})
	count, err = store.Save(context.Background(), "s1", msgs, count)
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if count != 3 {
		t.Fatalf("second save count: got %d, want 3", count)
	}
	if got := store.saveLens[1]; got != 1 {
		t.Fatalf("second save delta: got %d, want 1 (hint should restrict to new tail)", got)
	}
}

func TestMessageStore_Save_TruncationDropsOrphanTail(t *testing.T) {
	store := newFakeMessageStore()
	msgs := []Message{
		{Role: "system"},
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
	}
	_, _ = store.Save(context.Background(), "s1", msgs, 0)

	// Pruner shrinks msgs to 2 entries; store must drop entries 2,3.
	pruned := msgs[:2]
	count, err := store.Save(context.Background(), "s1", pruned, 4)
	if err != nil {
		t.Fatalf("truncation save: %v", err)
	}
	if count != 2 {
		t.Fatalf("count after truncation: got %d, want 2", count)
	}
	loaded, _ := store.Load(context.Background(), "s1")
	if len(loaded) != 2 {
		t.Fatalf("load after truncation: got %d, want 2", len(loaded))
	}
}

func TestMessageStore_Delete_RemovesAllRows(t *testing.T) {
	store := newFakeMessageStore()
	_, _ = store.Save(context.Background(), "s1", []Message{{Role: "user"}}, 0)
	if err := store.Delete(context.Background(), "s1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	loaded, _ := store.Load(context.Background(), "s1")
	if len(loaded) != 0 {
		t.Fatalf("load after delete: got %d, want 0", len(loaded))
	}
}

func TestWithMySQLMessageStore_AppliesOption(t *testing.T) {
	store := newFakeMessageStore()
	var cfg mysqlOptions
	WithMySQLMessageStore(store)(&cfg)
	if cfg.messageStore == nil {
		t.Fatal("messageStore not set on options")
	}
}
