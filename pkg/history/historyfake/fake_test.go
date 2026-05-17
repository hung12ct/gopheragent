package historyfake_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/history/historyfake"
)

// Compile-time interface satisfaction check.
var _ agent.SessionManager = (*historyfake.SessionManager)(nil)

func TestSessionManager_SeedAndGet(t *testing.T) {
	sm := historyfake.NewSessionManager().Seed("s1", []history.Message{
		{Role: "system", Content: "be helpful"},
		{Role: "user", Content: "hello"},
	})

	msgs, _ := sm.History(context.Background(), "s1")
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Content != "hello" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	if got := sm.Calls().History; got != 1 {
		t.Fatalf("expected 1 History call, got %d", got)
	}
}

func TestSessionManager_SetIsolated(t *testing.T) {
	sm := historyfake.NewSessionManager()
	msgs := []history.Message{{Role: "user", Content: "x"}}
	sm.SaveHistory(context.Background(), "s1", msgs)

	// Mutating input should not affect stored state.
	msgs[0].Content = "MUTATED"
	got, _ := sm.History(context.Background(), "s1")
	if got[0].Content != "x" {
		t.Fatalf("fake leaked input slice mutation: %q", got[0].Content)
	}
}

func TestSessionManager_SaveErr(t *testing.T) {
	sm := historyfake.NewSessionManager()
	sm.SaveErr = errors.New("boom")
	if err := sm.SaveHistory(context.Background(), "s1", nil); err == nil || err.Error() != "boom" {
		t.Fatalf("expected boom from SaveHistory, got %v", err)
	}
}

func TestSessionManager_DeleteClearsState(t *testing.T) {
	sm := historyfake.NewSessionManager().
		Seed("s1", []history.Message{{Role: "user", Content: "x"}})
	if err := sm.Delete(context.Background(), "s1"); err != nil {
		t.Fatalf("unexpected delete error: %v", err)
	}
	if got, _ := sm.History(context.Background(), "s1"); got != nil {
		t.Fatalf("expected nil after delete, got %+v", got)
	}
}

func TestSessionManager_Fork(t *testing.T) {
	sm := historyfake.NewSessionManager().Seed("s1", []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
	})
	newKey, err := sm.Fork(context.Background(), "s1", 2)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	forked, _ := sm.History(context.Background(), newKey)
	if len(forked) != 2 {
		t.Fatalf("expected 2 messages in fork, got %d", len(forked))
	}
}
