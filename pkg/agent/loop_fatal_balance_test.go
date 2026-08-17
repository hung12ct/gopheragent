package agent

import (
	"context"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// repeatCalls builds n tool calls that are byte-identical except for their
// provider call ID, so the anti-loop detector trips partway through the wave
// while the calls that already ran hold real results.
func repeatCalls(n int) []PendingToolCall {
	out := make([]PendingToolCall, n)
	for i := range n {
		out[i] = PendingToolCall{
			ID:       "dup" + string(rune('0'+i)),
			Name:     "counter",
			ArgsJSON: `{"same":true}`,
		}
	}
	return out
}

// assertToolCallsBalanced fails when any tool_call in msgs lacks a tool
// message carrying the matching ToolCallID. Providers reject an unbalanced
// transcript, so this is the property the saved history must always hold.
func assertToolCallsBalanced(t *testing.T, msgs []history.Message) {
	t.Helper()
	replied := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" {
			replied[m.ToolCallID] = true
		}
	}
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if !replied[tc.ID] {
				t.Fatalf("tool_call %q has no matching tool result in saved history (%d messages)", tc.ID, len(msgs))
			}
		}
	}
}

// A wave aborted by the anti-loop detector used to save the assistant
// message with its tool_calls and no tool results at all, discarding the
// results that had already completed. The saved transcript must instead be
// balanced, with completed results preserved.
func TestFatalWave_SavesBalancedHistory(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: repeatCalls(6)},
		{Content: "final"},
	}}
	loop, sm := setup(provider, ct)

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err == nil {
		t.Fatal("expected the anti-loop detector to abort the turn")
	}

	msgs, err := sm.History(context.Background(), "s1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	assertToolCallsBalanced(t, msgs)

	// The abort must not throw away work that already landed: at least one
	// call completed before the detector tripped at loopKillThreshold.
	var real, synthetic int
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		if m.Content == fatalAbortToolReason {
			synthetic++
			continue
		}
		real++
	}
	if real == 0 {
		t.Fatalf("every tool result was synthetic; completed work was discarded (%d synthetic)", synthetic)
	}
	if got := real + synthetic; got != 6 {
		t.Fatalf("tool results = %d, want 6 (real=%d synthetic=%d)", got, real, synthetic)
	}
}

// synthesizeMissingToolErrors must never overwrite a recorded result.
func TestSynthesizeMissingToolErrors_PreservesCompleted(t *testing.T) {
	calls := repeatCalls(3)
	toolMsgs := map[string]history.Message{
		calls[1].ID: {Role: "tool", Content: "kept", ToolCallID: calls[1].ID},
	}

	synthesizeMissingToolErrors(toolMsgs, calls, fatalAbortToolReason)

	if len(toolMsgs) != 3 {
		t.Fatalf("len(toolMsgs) = %d, want 3", len(toolMsgs))
	}
	if got := toolMsgs[calls[1].ID].Content; got != "kept" {
		t.Fatalf("completed result overwritten: %q", got)
	}
	for _, i := range []int{0, 2} {
		m := toolMsgs[calls[i].ID]
		if m.Content != fatalAbortToolReason || !m.IsError || m.ToolCallID != calls[i].ID {
			t.Fatalf("call %d: unexpected synthetic message %+v", i, m)
		}
	}
}

// An empty per-wave map is the abort-before-any-dispatch case: every call
// still needs a reply.
func TestSynthesizeMissingToolErrors_FillsEmptyMap(t *testing.T) {
	calls := repeatCalls(4)
	toolMsgs := map[string]history.Message{}

	synthesizeMissingToolErrors(toolMsgs, calls, fatalAbortToolReason)

	if len(toolMsgs) != len(calls) {
		t.Fatalf("len(toolMsgs) = %d, want %d", len(toolMsgs), len(calls))
	}
	var msgs []history.Message
	msgs = append(msgs, history.Message{Role: "assistant", ToolCalls: toolCallsOf(calls)})
	msgs = appendToolResultsInOrder(msgs, calls, toolMsgs)
	assertToolCallsBalanced(t, msgs)
}

// toolCallsOf mirrors the pending calls into the history shape the assistant
// message carries, so a balance assertion can run over a hand-built slice.
func toolCallsOf(calls []PendingToolCall) []history.ToolCall {
	out := make([]history.ToolCall, 0, len(calls))
	for _, tc := range calls {
		out = append(out, history.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.ArgsJSON})
	}
	return out
}
