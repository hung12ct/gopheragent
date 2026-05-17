package history

import "testing"

func TestSnapToSafeBoundary_DanglingAssistantIsDropped(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do it"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "t"}}},
	}
	// Cutting right after the assistant would leave a dangling tool_call.
	got := snapToSafeBoundary(msgs, 3)
	if got != 2 {
		t.Fatalf("expected snap back to 2 (drop dangling assistant), got %d", got)
	}
}

func TestSnapToSafeBoundary_PartialToolResultsDropGroup(t *testing.T) {
	msgs := []Message{
		{Role: "system"},
		{Role: "user", Content: "run"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Name: "t"},
			{ID: "c2", Name: "t"},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		// c2's result is missing → the whole assistant+tool group must be dropped
	}
	got := snapToSafeBoundary(msgs, 4)
	if got != 2 {
		t.Fatalf("expected snap back to 2 (drop unresolved group), got %d", got)
	}
}

func TestSnapToSafeBoundary_CompleteToolGroupIsKept(t *testing.T) {
	msgs := []Message{
		{Role: "system"},
		{Role: "user", Content: "run"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Name: "t"},
			{ID: "c2", Name: "t"},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "ok1"},
		{Role: "tool", ToolCallID: "c2", Content: "ok2"},
	}
	got := snapToSafeBoundary(msgs, 5)
	if got != 5 {
		t.Fatalf("fully resolved tool group must be kept intact, got %d", got)
	}
}

func TestSnapToSafeBoundary_ClampAndNegative(t *testing.T) {
	msgs := []Message{{Role: "system"}, {Role: "user", Content: "hi"}}
	if got := snapToSafeBoundary(msgs, 99); got != 2 {
		t.Fatalf("oversized atIndex should clamp to len, got %d", got)
	}
	if got := snapToSafeBoundary(msgs, -5); got != 0 {
		t.Fatalf("negative atIndex should clamp to 0, got %d", got)
	}
}

func TestSafeTruncate_PublicAliasMatchesInternal(t *testing.T) {
	// SafeTruncate is the public surface used by agent.Regenerate /
	// agent.Continue; it must remain a thin alias over snapToSafeBoundary so
	// the in-place rewind and Fork paths can't drift.
	msgs := []Message{
		{Role: "system"},
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "t"}}},
	}
	if got, want := SafeTruncate(msgs, 3), snapToSafeBoundary(msgs, 3); got != want {
		t.Fatalf("SafeTruncate diverged from snapToSafeBoundary: got %d want %d", got, want)
	}
	if got := SafeTruncate(msgs, 2); got != 2 {
		t.Fatalf("safe boundary should be returned unchanged, got %d", got)
	}
}

func TestInMemSessionManager_Fork_AutoSnapsOnDanglingAssistant(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	ctx := t.Context()

	msgs, _ := sm.History(ctx, "parent")
	msgs = append(msgs,
		Message{Role: "user", Content: "search something"},
		Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "search"}}},
		Message{Role: "tool", ToolCallID: "c1", Content: "results"},
		Message{Role: "assistant", Content: "final"},
	)
	sm.SaveHistory(ctx, "parent", msgs)

	// Ask to fork at index 3 (right after the assistant-with-tool-calls).
	// Snap should back off to 2 so the fork doesn't carry a dangling tool_call.
	newKey, err := sm.Fork(ctx, "parent", 3)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	forked, _ := sm.History(ctx, newKey)
	if len(forked) != 2 {
		t.Fatalf("expected 2 messages after snap, got %d: %+v", len(forked), forked)
	}
	if forked[1].Role != "user" {
		t.Fatalf("expected last kept message to be user, got role=%q", forked[1].Role)
	}
}
