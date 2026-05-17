package agent

import (
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

func TestForkAtLastUser_BranchesBeforeLastUserTurn(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	ctx := t.Context()

	msgs, _ := sm.History(ctx, "parent")
	msgs = append(msgs,
		history.Message{Role: "user", Content: "first"},
		history.Message{Role: "assistant", Content: "answer 1"},
		history.Message{Role: "user", Content: "second — redo this"},
		history.Message{Role: "assistant", Content: "answer 2"},
	)
	sm.SaveHistory(ctx, "parent", msgs)

	newKey, err := ForkAtLastUser(ctx, sm, "parent")
	if err != nil {
		t.Fatalf("ForkAtLastUser: %v", err)
	}

	forked, _ := sm.History(ctx, newKey)
	// Expect [system, user "first", assistant "answer 1"] — everything before the second user turn.
	if len(forked) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(forked), forked)
	}
	if forked[1].Content != "first" || forked[2].Content != "answer 1" {
		t.Fatalf("wrong prefix: %+v", forked)
	}
}

func TestForkAtLastUser_ErrorsWhenNoUserMessages(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	ctx := t.Context()

	// Session contains only the system message — no user turns to fork before.
	sm.SaveHistory(ctx, "empty", []history.Message{{Role: "system", Content: "sys"}})

	if _, err := ForkAtLastUser(ctx, sm, "empty"); err == nil {
		t.Fatal("expected error, got nil")
	} else if !strings.Contains(err.Error(), "no user messages") {
		t.Fatalf("unexpected error: %v", err)
	}
}
