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

	msgs := sm.GetHistory(ctx, "parent")
	msgs = append(msgs,
		Message{Role: "user", Content: "a"},
		Message{Role: "assistant", Content: "A"},
		Message{Role: "user", Content: "b"},
	)
	sm.SetHistory(ctx, "parent", msgs)
	if err := sm.Save(ctx, "parent"); err != nil {
		t.Fatalf("Save parent: %v", err)
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
	reloaded := sm2.GetHistory(ctx, newKey)
	if len(reloaded) != 3 {
		t.Fatalf("expected 3 persisted messages, got %d", len(reloaded))
	}
	if reloaded[1].Content != "a" || reloaded[2].Content != "A" {
		t.Fatalf("persisted prefix corrupted: %+v", reloaded)
	}

	// The parent must still have all four messages.
	parent := sm2.GetHistory(ctx, "parent")
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
