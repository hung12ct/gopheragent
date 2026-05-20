package memory

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInMemStore_PutAndList(t *testing.T) {
	s := NewInMemStore()
	ctx := context.Background()

	if err := s.Put(ctx, "user-1", Note{Key: "k1", Content: "first"}); err != nil {
		t.Fatalf("put k1: %v", err)
	}
	if err := s.Put(ctx, "user-1", Note{Key: "k2", Content: "second"}); err != nil {
		t.Fatalf("put k2: %v", err)
	}

	notes, err := s.List(ctx, "user-1", ListOpts{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes, got %d", len(notes))
	}
	if notes[0].Key != "k2" {
		t.Fatalf("expected newest-first ordering (k2), got %q", notes[0].Key)
	}
}

func TestInMemStore_UpdatePreservesCreatedAt(t *testing.T) {
	s := NewInMemStore()
	ctx := context.Background()

	_ = s.Put(ctx, "u", Note{Key: "k", Content: "v1"})
	first, _ := s.List(ctx, "u", ListOpts{})
	created := first[0].CreatedAt

	_ = s.Put(ctx, "u", Note{Key: "k", Content: "v2"})
	second, _ := s.List(ctx, "u", ListOpts{})
	if !second[0].CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt should persist across updates: was %v now %v", created, second[0].CreatedAt)
	}
	if second[0].Content != "v2" {
		t.Fatalf("Content should update: got %q", second[0].Content)
	}
	if !second[0].UpdatedAt.After(created) && !second[0].UpdatedAt.Equal(created) {
		t.Fatalf("UpdatedAt should advance: was %v now %v", created, second[0].UpdatedAt)
	}
}

func TestInMemStore_Delete(t *testing.T) {
	s := NewInMemStore()
	ctx := context.Background()
	_ = s.Put(ctx, "u", Note{Key: "k", Content: "v"})
	_ = s.Delete(ctx, "u", "k")
	notes, _ := s.List(ctx, "u", ListOpts{})
	if len(notes) != 0 {
		t.Fatalf("expected empty after delete, got %d", len(notes))
	}
	// Deleting again is a no-op.
	if err := s.Delete(ctx, "u", "k"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestInMemStore_ScopeIsolation(t *testing.T) {
	s := NewInMemStore()
	ctx := context.Background()
	_ = s.Put(ctx, "alice", Note{Key: "k", Content: "alice-data"})
	_ = s.Put(ctx, "bob", Note{Key: "k", Content: "bob-data"})

	aliceNotes, _ := s.List(ctx, "alice", ListOpts{})
	if len(aliceNotes) != 1 || aliceNotes[0].Content != "alice-data" {
		t.Fatalf("scope leak: alice got %+v", aliceNotes)
	}
	bobNotes, _ := s.List(ctx, "bob", ListOpts{})
	if len(bobNotes) != 1 || bobNotes[0].Content != "bob-data" {
		t.Fatalf("scope leak: bob got %+v", bobNotes)
	}
}

func TestInMemStore_EmptyKeyRejected(t *testing.T) {
	s := NewInMemStore()
	err := s.Put(context.Background(), "u", Note{Content: "no key"})
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestInMemStore_ConcurrentPutList(t *testing.T) {
	s := NewInMemStore()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = s.Put(ctx, "u", Note{Key: keyOf(i), Content: "v"})
		}(i)
		go func() {
			defer wg.Done()
			_, _ = s.List(ctx, "u", ListOpts{})
		}()
	}
	wg.Wait()
	notes, _ := s.List(ctx, "u", ListOpts{})
	if len(notes) != 100 {
		t.Fatalf("expected 100 notes after concurrent writes, got %d", len(notes))
	}
}

func TestFormatNotes_Empty(t *testing.T) {
	if got := FormatNotes(nil); got != "" {
		t.Fatalf("expected empty for nil, got %q", got)
	}
	if got := FormatNotes([]Note{}); got != "" {
		t.Fatalf("expected empty for empty slice, got %q", got)
	}
}

func TestFormatNotes_RendersBulletList(t *testing.T) {
	out := FormatNotes([]Note{
		{Content: "user prefers metric units"},
		{Content: "jira workspace is acme-corp"},
	})
	if !strings.Contains(out, "Long-term memory") {
		t.Fatalf("missing header: %q", out)
	}
	if !strings.Contains(out, "- user prefers metric units") {
		t.Fatalf("missing first bullet: %q", out)
	}
	if !strings.Contains(out, "- jira workspace is acme-corp") {
		t.Fatalf("missing second bullet: %q", out)
	}
}

func TestInMemStore_DefaultCapApplied(t *testing.T) {
	s := NewInMemStore() // no option — should default to DefaultMaxNotesPerScope.
	ctx := context.Background()
	for i := range DefaultMaxNotesPerScope + 5 {
		_ = s.Put(ctx, "u", Note{Key: keyOf(i), Content: "v"})
	}
	notes, _ := s.List(ctx, "u", ListOpts{})
	if len(notes) != DefaultMaxNotesPerScope {
		t.Fatalf("default cap not applied: expected %d, got %d", DefaultMaxNotesPerScope, len(notes))
	}
}

func TestInMemStore_ExplicitZeroDisablesCap(t *testing.T) {
	s := NewInMemStore(WithMaxNotesPerScope(0)) // explicit opt-out.
	ctx := context.Background()
	for i := range DefaultMaxNotesPerScope + 5 {
		_ = s.Put(ctx, "u", Note{Key: keyOf(i), Content: "v"})
	}
	notes, _ := s.List(ctx, "u", ListOpts{})
	if len(notes) != DefaultMaxNotesPerScope+5 {
		t.Fatalf("WithMaxNotesPerScope(0) should disable cap: got %d", len(notes))
	}
}

func TestInMemStore_PerScopeCapEvictsOldest(t *testing.T) {
	s := NewInMemStore(WithMaxNotesPerScope(3))
	ctx := context.Background()

	// Stamp UpdatedAt by inserting in order; eviction targets oldest.
	for i := range 5 {
		if err := s.Put(ctx, "u", Note{Key: keyOf(i), Content: "v"}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	notes, _ := s.List(ctx, "u", ListOpts{})
	if len(notes) != 3 {
		t.Fatalf("expected cap=3, got %d", len(notes))
	}
	// Keys 0 and 1 should have been evicted (oldest UpdatedAt).
	keys := make(map[string]bool, len(notes))
	for _, n := range notes {
		keys[n.Key] = true
	}
	for _, want := range []string{"k-2", "k-3", "k-4"} {
		if !keys[want] {
			t.Fatalf("expected %q in store, got keys %v", want, keys)
		}
	}
}

func TestInMemStore_UpdatesDoNotEvict(t *testing.T) {
	s := NewInMemStore(WithMaxNotesPerScope(2))
	ctx := context.Background()
	_ = s.Put(ctx, "u", Note{Key: "a", Content: "v1"})
	_ = s.Put(ctx, "u", Note{Key: "b", Content: "v1"})
	// Updating an existing key must NOT trip the eviction path.
	_ = s.Put(ctx, "u", Note{Key: "a", Content: "v2"})
	notes, _ := s.List(ctx, "u", ListOpts{})
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes after update, got %d", len(notes))
	}
}

func TestInMemStore_ListLimit(t *testing.T) {
	s := NewInMemStore()
	ctx := context.Background()
	for i := range 10 {
		_ = s.Put(ctx, "u", Note{Key: keyOf(i), Content: "v"})
	}
	notes, _ := s.List(ctx, "u", ListOpts{Limit: 3})
	if len(notes) != 3 {
		t.Fatalf("expected 3 notes via Limit, got %d", len(notes))
	}
	// Most recent first: k-9, k-8, k-7
	if notes[0].Key != "k-9" {
		t.Fatalf("expected newest first, got %q", notes[0].Key)
	}
}

func TestInMemStore_ListTagsAndFilter(t *testing.T) {
	s := NewInMemStore()
	ctx := context.Background()
	_ = s.Put(ctx, "u", Note{Key: "a", Content: "v", Tags: []string{"preference", "ui"}})
	_ = s.Put(ctx, "u", Note{Key: "b", Content: "v", Tags: []string{"fact"}})
	_ = s.Put(ctx, "u", Note{Key: "c", Content: "v", Tags: []string{"preference"}})

	got, _ := s.List(ctx, "u", ListOpts{Tags: []string{"preference"}})
	if len(got) != 2 {
		t.Fatalf("expected 2 preference-tagged notes, got %d", len(got))
	}
	got2, _ := s.List(ctx, "u", ListOpts{Tags: []string{"preference", "ui"}})
	if len(got2) != 1 || got2[0].Key != "a" {
		t.Fatalf("expected AND filter to match only 'a', got %+v", got2)
	}
}

func TestInMemStore_ListUpdatedAfter(t *testing.T) {
	s := NewInMemStore()
	ctx := context.Background()
	_ = s.Put(ctx, "u", Note{Key: "old", Content: "v"})
	// Cutoff between the two puts.
	cutoff := time.Now().UTC()
	// Make sure we cross at least one nanosecond before the next put.
	time.Sleep(time.Millisecond)
	_ = s.Put(ctx, "u", Note{Key: "new", Content: "v"})

	got, _ := s.List(ctx, "u", ListOpts{UpdatedAfter: cutoff})
	if len(got) != 1 || got[0].Key != "new" {
		t.Fatalf("UpdatedAfter filter wrong: got %+v", got)
	}
}

func TestInMemStore_ReplaceAll(t *testing.T) {
	s := NewInMemStore()
	ctx := context.Background()
	_ = s.Put(ctx, "u", Note{Key: "keep", Content: "old-content"})
	_ = s.Put(ctx, "u", Note{Key: "drop", Content: "to be removed"})

	priorCreated := func(key string) time.Time {
		notes, _ := s.List(ctx, "u", ListOpts{})
		for _, n := range notes {
			if n.Key == key {
				return n.CreatedAt
			}
		}
		return time.Time{}
	}
	keepCreated := priorCreated("keep")

	err := s.ReplaceAll(ctx, "u", []Note{
		{Key: "keep", Content: "updated"},
		{Key: "fresh", Content: "new"},
	})
	if err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	notes, _ := s.List(ctx, "u", ListOpts{})
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes after ReplaceAll, got %d", len(notes))
	}
	got := map[string]Note{}
	for _, n := range notes {
		got[n.Key] = n
	}
	if got["keep"].Content != "updated" {
		t.Fatalf("expected 'keep' content updated, got %q", got["keep"].Content)
	}
	if !got["keep"].CreatedAt.Equal(keepCreated) {
		t.Fatalf("expected 'keep' CreatedAt preserved across ReplaceAll: before=%v after=%v", keepCreated, got["keep"].CreatedAt)
	}
	if _, exists := got["drop"]; exists {
		t.Fatalf("expected 'drop' removed, still present")
	}
}

func TestInMemStore_ReplaceAllEmptyClears(t *testing.T) {
	s := NewInMemStore()
	ctx := context.Background()
	_ = s.Put(ctx, "u", Note{Key: "a", Content: "v"})
	if err := s.ReplaceAll(ctx, "u", nil); err != nil {
		t.Fatalf("ReplaceAll nil: %v", err)
	}
	notes, _ := s.List(ctx, "u", ListOpts{})
	if len(notes) != 0 {
		t.Fatalf("expected scope cleared, got %d", len(notes))
	}
}

func TestInMemStore_ReplaceAllRespectsCap(t *testing.T) {
	s := NewInMemStore(WithMaxNotesPerScope(2))
	ctx := context.Background()
	err := s.ReplaceAll(ctx, "u", []Note{
		{Key: "a", Content: "v"},
		{Key: "b", Content: "v"},
		{Key: "c", Content: "v"},
		{Key: "d", Content: "v"},
	})
	if err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	notes, _ := s.List(ctx, "u", ListOpts{})
	if len(notes) != 2 {
		t.Fatalf("expected ReplaceAll to enforce cap=2, got %d", len(notes))
	}
}

func TestInMemStore_ReplaceAllRejectsEmptyKey(t *testing.T) {
	s := NewInMemStore()
	err := s.ReplaceAll(context.Background(), "u", []Note{{Content: "no key"}})
	if err == nil {
		t.Fatal("expected error for empty key in ReplaceAll")
	}
}

func TestFormatNotes_HeaderShapeStable(t *testing.T) {
	// Pin the FormatNotes header bytes. trimToTokenBudget in pkg/agent
	// hard-codes the header length to bound the loader's prompt; this
	// test catches silent drift if anyone reworks the header text.
	got := FormatNotes([]Note{{Content: "x"}})
	if !strings.HasSuffix(got, "- x\n") {
		t.Fatalf("expected bullet at end, got %q", got)
	}
	header := strings.TrimSuffix(got, "- x\n")
	const expected = 145
	if len(header) != expected {
		t.Fatalf("FormatNotes header changed length: expected %d, got %d (%q) — update agent.trimToTokenBudget's headerChars constant", expected, len(header), header)
	}
}

func TestFormatNotes_SkipsEmptyContent(t *testing.T) {
	out := FormatNotes([]Note{
		{Content: "kept"},
		{Content: ""},
	})
	if strings.Count(out, "- ") != 1 {
		t.Fatalf("expected exactly one bullet, got %q", out)
	}
}

func keyOf(i int) string {
	return "k-" + strconv.Itoa(i)
}
