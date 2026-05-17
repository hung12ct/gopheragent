package history

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestFileSessionManager_Fork_PersistsCopy(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewFileSessionManager(dir, "sys")
	if err != nil {
		t.Fatalf("NewFileSessionManager: %v", err)
	}
	ctx := context.Background()

	msgs, _ := sm.History(ctx, "parent")
	msgs = append(msgs,
		Message{Role: "user", Content: "a"},
		Message{Role: "assistant", Content: "A"},
		Message{Role: "user", Content: "b"},
	)
	if err := sm.SaveHistory(ctx, "parent", msgs); err != nil {
		t.Fatalf("SaveHistory parent: %v", err)
	}

	newKey, err := sm.Fork(ctx, "parent", 3) // keep [system, user "a", assistant "A"]
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	// Verify the forked JSON file exists on disk.
	if _, err := os.Stat(dir + "/" + newKey + ".json"); err != nil {
		t.Fatalf("forked session file missing: %v", err)
	}

	// Drop the cache and reload from disk to prove persistence.
	sm2, err := NewFileSessionManager(dir, "sys")
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	reloaded, _ := sm2.History(ctx, newKey)
	if len(reloaded) != 3 {
		t.Fatalf("expected 3 persisted messages, got %d", len(reloaded))
	}
	if reloaded[1].Content != "a" || reloaded[2].Content != "A" {
		t.Fatalf("persisted prefix corrupted: %+v", reloaded)
	}

	// The parent must still have all four messages.
	parent, _ := sm2.History(ctx, "parent")
	if len(parent) != 4 {
		t.Fatalf("parent tampered: expected 4 messages, got %d", len(parent))
	}
}

func TestFileSessionManager_Fork_RejectsMissingSource(t *testing.T) {
	sm, err := NewFileSessionManager(t.TempDir(), "sys")
	if err != nil {
		t.Fatalf("NewFileSessionManager: %v", err)
	}
	if _, err := sm.Fork(context.Background(), "unknown", 1); err == nil {
		t.Fatal("expected error for missing source session")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFileSessionManager_SetTitle_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewFileSessionManager(dir, "sys")
	if err != nil {
		t.Fatalf("NewFileSessionManager: %v", err)
	}
	ctx := context.Background()

	if err := sm.SaveHistory(ctx, "s1", []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
	}); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}
	if err := sm.SetTitle(ctx, "s1", "Customer revenue audit"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}

	// Re-open from disk to prove the title persists in the sidecar.
	sm2, err := NewFileSessionManager(dir, "sys")
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	got, _ := sm2.Query(ctx, "", SessionQueryOpts{})
	if len(got) != 1 {
		t.Fatalf("expected 1 session, got %d", len(got))
	}
	if got[0].Title != "Customer revenue audit" {
		t.Fatalf("title not persisted, got %q", got[0].Title)
	}

	// Empty title clears the sidecar entry.
	if err := sm2.SetTitle(ctx, "s1", ""); err != nil {
		t.Fatalf("SetTitle clear: %v", err)
	}
	sm3, _ := NewFileSessionManager(dir, "sys")
	got, _ = sm3.Query(ctx, "", SessionQueryOpts{})
	if len(got) != 1 || got[0].Title != "" {
		t.Fatalf("empty SetTitle did not clear, got %+v", got)
	}
}
