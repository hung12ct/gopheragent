package history

import (
	"context"
	"testing"
	"time"
)

// seedSession simulates what the agent loop does: get existing history,
// append a user message, and store back.
func seedSession(sm *InMemSessionManager, key string, userMsg string) {
	ctx := context.Background()
	msgs := sm.GetHistory(ctx, key) // gets [system] for new session
	msgs = append(msgs, Message{Role: "user", Content: userMsg})
	sm.SetHistory(ctx, key, msgs)
}

func TestInMemSessionManager_BasicGetSet(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	ctx := context.Background()

	seedSession(sm, "s1", "hello")
	msgs := sm.GetHistory(ctx, "s1")
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
	msgs := sm.GetHistory(context.Background(), "s1")
	if len(msgs) < 2 {
		t.Fatal("session should exist before TTL")
	}

	// Wait beyond TTL without any access → eviction should fire
	time.Sleep(200 * time.Millisecond)

	// Session should have been evicted — returns fresh system-only message
	msgs = sm.GetHistory(context.Background(), "s1")
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
		msgs := sm.GetHistory(context.Background(), "s1")
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

	msgs := sm.GetHistory(context.Background(), "s1")
	if len(msgs) < 2 {
		t.Fatal("session with TTL=0 should never expire")
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
			sm.GetHistory(context.Background(), "s1")
			time.Sleep(time.Millisecond)
		}
	}()
	<-done // no race or panic = pass
}
