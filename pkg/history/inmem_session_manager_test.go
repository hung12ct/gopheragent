package history

import (
	"context"
	"strings"
	"testing"
	"time"
)

// seedSession simulates what the agent loop does: get existing history,
// append a user message, and store back.
func seedSession(sm *InMemSessionManager, key string, userMsg string) {
	ctx := context.Background()
	msgs, _ := sm.History(ctx, key) // gets [system] for new session
	msgs = append(msgs, Message{Role: "user", Content: userMsg})
	sm.SaveHistory(ctx, key, msgs)
}

func TestInMemSessionManager_BasicGetSet(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	ctx := context.Background()

	seedSession(sm, "s1", "hello")
	msgs, _ := sm.History(ctx, "s1")
	if len(msgs) < 2 { // system + user
		t.Fatalf("expected system+user, got %d messages", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatal("first message should be system")
	}
}

func TestInMemSessionManager_TTL_Eviction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sm := NewInMemSessionManager("sys").
		WithTTL(50 * time.Millisecond).
		StartCleanup(ctx, 20*time.Millisecond)

	seedSession(sm, "s1", "hi")

	// Session should exist immediately
	msgs, _ := sm.History(context.Background(), "s1")
	if len(msgs) < 2 {
		t.Fatal("session should exist before TTL")
	}

	// Wait beyond TTL without any access → eviction should fire
	time.Sleep(200 * time.Millisecond)

	// Session should have been evicted — returns fresh system-only message
	msgs, _ = sm.History(context.Background(), "s1")
	if len(msgs) != 1 || msgs[0].Role != "system" {
		t.Fatalf("expected evicted session to return only system message, got %d msgs", len(msgs))
	}
}

func TestInMemSessionManager_TTL_TouchOnRead_PreventsExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sm := NewInMemSessionManager("sys").
		WithTTL(80 * time.Millisecond).
		StartCleanup(ctx, 30*time.Millisecond)

	seedSession(sm, "s1", "hi")

	// Keep reading every 40ms — each read resets idle timer, preventing eviction
	for i := 0; i < 4; i++ {
		time.Sleep(40 * time.Millisecond)
		msgs, _ := sm.History(context.Background(), "s1")
		if len(msgs) < 2 {
			t.Fatalf("session should NOT be evicted while actively read (iteration %d)", i)
		}
	}
}

func TestInMemSessionManager_TTL_Zero_NeverExpires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sm := NewInMemSessionManager("sys") // TTL = 0 (disabled)
	sm.StartCleanup(ctx, 10*time.Millisecond)

	seedSession(sm, "s1", "hi")
	time.Sleep(50 * time.Millisecond)

	msgs, _ := sm.History(context.Background(), "s1")
	if len(msgs) < 2 {
		t.Fatal("session with TTL=0 should never expire")
	}
}

func TestInMemSessionManager_Fork_CopiesPrefixAndIsolates(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	ctx := context.Background()

	// Build a session: [system, user "a", assistant "A", user "b", assistant "B"]
	msgs, _ := sm.History(ctx, "parent")
	msgs = append(msgs,
		Message{Role: "user", Content: "a"},
		Message{Role: "assistant", Content: "A"},
		Message{Role: "user", Content: "b"},
		Message{Role: "assistant", Content: "B"},
	)
	sm.SaveHistory(ctx, "parent", msgs)

	// Fork after the first user/assistant exchange: keep 3 messages (system, user "a", assistant "A").
	newKey, err := sm.Fork(ctx, "parent", 3)
	if err != nil {
		t.Fatalf("Fork failed: %v", err)
	}
	if !strings.HasPrefix(newKey, "parent-fork-") {
		t.Fatalf("unexpected fork key format: %q", newKey)
	}

	forked, _ := sm.History(ctx, newKey)
	if len(forked) != 3 {
		t.Fatalf("forked session: expected 3 messages, got %d", len(forked))
	}
	if forked[1].Content != "a" || forked[2].Content != "A" {
		t.Fatalf("forked prefix corrupted: %+v", forked)
	}

	// Mutating the forked branch must not leak into the parent.
	forked = append(forked, Message{Role: "user", Content: "different"})
	sm.SaveHistory(ctx, newKey, forked)

	parent, _ := sm.History(ctx, "parent")
	if len(parent) != 5 {
		t.Fatalf("parent session tampered: expected 5 messages, got %d", len(parent))
	}
	if parent[4].Content != "B" {
		t.Fatalf("parent tail mutated: got %q", parent[4].Content)
	}
}

func TestInMemSessionManager_Fork_ClampsAndValidates(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	ctx := context.Background()
	seedSession(sm, "parent", "hello")

	// atIndex beyond length clamps to len(messages).
	newKey, err := sm.Fork(ctx, "parent", 999)
	if err != nil {
		t.Fatalf("Fork with oversized atIndex: %v", err)
	}
	forked, _ := sm.History(ctx, newKey)
	if len(forked) != 2 {
		t.Fatalf("expected full copy (2 msgs), got %d", len(forked))
	}

	// atIndex == 1 keeps only the system message — the common "clean slate, same persona" fork.
	systemKey, err := sm.Fork(ctx, "parent", 1)
	if err != nil {
		t.Fatalf("Fork with atIndex=1: %v", err)
	}
	systemOnly, _ := sm.History(ctx, systemKey)
	if len(systemOnly) != 1 || systemOnly[0].Role != "system" {
		t.Fatalf("expected system-only, got %+v", systemOnly)
	}

	// Negative atIndex is an error.
	if _, err := sm.Fork(ctx, "parent", -1); err == nil {
		t.Fatal("expected error for negative atIndex")
	}

	// Unknown source session is an error.
	if _, err := sm.Fork(ctx, "nope", 1); err == nil {
		t.Fatal("expected error for missing source session")
	}
}

func TestInMemSessionManager_Fork_CopiesBehaviorSummary(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	ctx := context.Background()

	seedSession(sm, "parent", "hi")
	if err := sm.UpdateBehaviorSummary("parent", "user prefers terse answers"); err != nil {
		t.Fatalf("UpdateBehaviorSummary: %v", err)
	}

	newKey, err := sm.Fork(ctx, "parent", 2)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	forked, _ := sm.History(ctx, newKey)
	if !strings.Contains(forked[0].Content, "terse answers") {
		t.Fatalf("behavior summary not carried into fork; system content=%q", forked[0].Content)
	}
}

func TestInMemSessionManager_ConcurrentEvictionAndAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sm := NewInMemSessionManager("sys").
		WithTTL(30 * time.Millisecond).
		StartCleanup(ctx, 10*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			seedSession(sm, "s1", "msg")
			_, _ = sm.History(context.Background(), "s1")
			time.Sleep(time.Millisecond)
		}
	}()
	<-done // no race or panic = pass
}
