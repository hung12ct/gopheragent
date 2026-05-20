package memory

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
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

	notes, err := s.List(ctx, "user-1")
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
	first, _ := s.List(ctx, "u")
	created := first[0].CreatedAt

	_ = s.Put(ctx, "u", Note{Key: "k", Content: "v2"})
	second, _ := s.List(ctx, "u")
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
	notes, _ := s.List(ctx, "u")
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

	aliceNotes, _ := s.List(ctx, "alice")
	if len(aliceNotes) != 1 || aliceNotes[0].Content != "alice-data" {
		t.Fatalf("scope leak: alice got %+v", aliceNotes)
	}
	bobNotes, _ := s.List(ctx, "bob")
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
			_, _ = s.List(ctx, "u")
		}()
	}
	wg.Wait()
	notes, _ := s.List(ctx, "u")
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
