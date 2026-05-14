package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// drainStream collects every event from ch until ch closes. Helper for
// Regenerate / Continue tests that assert on the full transcript.
func drainStream(ch <-chan StreamEvent) []StreamEvent {
	var out []StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestRegenerate_ReplaysLastUserAndEmitsTransitionFrame(t *testing.T) {
	// Seed a completed turn the user wants to redo. The second scripted turn
	// is the new answer the regenerated run should produce.
	provider := &scriptProvider{turns: []LLMResult{
		{Content: "first answer"},
		{Content: "regenerated answer"},
	}}
	loop, sm := setup(provider)

	// Turn 1 — runs and persists [system, user, assistant].
	if _, err := loop.RunIteration(context.Background(), "s1", "tell me a joke"); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	before := sm.GetHistory(context.Background(), "s1")
	if got := len(before); got != 3 {
		t.Fatalf("expected 3 seeded messages, got %d: %+v", got, before)
	}

	// Regenerate must rewind to [system, user] (the user message stays in
	// the truncated prefix because the loop re-appends it).
	ch := make(chan StreamEvent, 16)
	if err := loop.Regenerate(context.Background(), "s1", ch); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	events := drainStream(ch)

	// First frame must be EventTypeRegenerated.
	if len(events) == 0 || events[0].Type != EventTypeRegenerated {
		t.Fatalf("expected first frame to be %q, got %+v", EventTypeRegenerated, events)
	}
	payload, ok := events[0].Payload().(RegeneratedEvent)
	if !ok {
		t.Fatalf("expected RegeneratedEvent payload, got %T", events[0].Payload())
	}
	if payload.PreviousAssistantIndex != 2 {
		t.Errorf("expected PreviousAssistantIndex=2, got %d", payload.PreviousAssistantIndex)
	}
	if payload.TruncatedAt != 1 {
		t.Errorf("expected TruncatedAt=1 (only [system] survives the rewind), got %d", payload.TruncatedAt)
	}

	// History must now end with the new assistant answer.
	after := sm.GetHistory(context.Background(), "s1")
	if last := after[len(after)-1]; last.Role != "assistant" || !strings.Contains(last.Content, "regenerated") {
		t.Fatalf("expected new assistant answer to be persisted, got %+v", last)
	}
}

func TestRegenerate_NoUserMessageReturnsSentinel(t *testing.T) {
	provider := &scriptProvider{}
	loop, _ := setup(provider)

	ch := make(chan StreamEvent, 4)
	err := loop.Regenerate(context.Background(), "empty", ch)
	if !errors.Is(err, ErrNothingToRegenerate) {
		t.Fatalf("expected ErrNothingToRegenerate, got %v", err)
	}
	// Channel must be closed so adopters ranging over it exit cleanly.
	if _, open := <-ch; open {
		t.Fatalf("expected streamChan to be closed on sentinel return")
	}
}

func TestContinue_ResumesFromInterruptedHistory(t *testing.T) {
	// Build a history snapshot that looks like a MaxIters-interrupted turn:
	// user is asking, the assistant produced one tool call + result, but no
	// final answer arrived. canContinue should accept it; the scripted
	// follow-up turn delivers the final content.
	provider := &scriptProvider{turns: []LLMResult{
		{Content: "finishing the thought"},
	}}
	loop, sm := setup(provider)
	ctx := context.Background()

	seed := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "summarize"},
		{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "c1", Name: "echo", Arguments: `"a"`}}},
		{Role: "tool", ToolCallID: "c1", Content: "echo:a"},
	}
	sm.SetHistory(ctx, "s1", seed)

	ch := make(chan StreamEvent, 16)
	if err := loop.Continue(ctx, "s1", ch); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	events := drainStream(ch)
	if len(events) == 0 || events[0].Type != EventTypeContinued {
		t.Fatalf("expected first frame %q, got %+v", EventTypeContinued, events)
	}
	payload, ok := events[0].Payload().(ContinuedEvent)
	if !ok {
		t.Fatalf("expected ContinuedEvent payload, got %T", events[0].Payload())
	}
	if payload.ContinuedFromIndex != 3 {
		t.Errorf("expected ContinuedFromIndex=3 (index of trailing tool result), got %d", payload.ContinuedFromIndex)
	}

	after := sm.GetHistory(ctx, "s1")
	last := after[len(after)-1]
	if last.Role != "assistant" || !strings.Contains(last.Content, "finishing") {
		t.Fatalf("expected final assistant answer appended, got %+v", last)
	}
}

func TestContinue_CleanFinalReturnsSentinel(t *testing.T) {
	provider := &scriptProvider{}
	loop, sm := setup(provider)
	ctx := context.Background()

	// Clean final-assistant ending — no dangling tool_calls, no tool tail.
	sm.SetHistory(ctx, "s1", []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "all done"},
	})

	ch := make(chan StreamEvent, 4)
	err := loop.Continue(ctx, "s1", ch)
	if !errors.Is(err, ErrNothingToContinue) {
		t.Fatalf("expected ErrNothingToContinue, got %v", err)
	}
	if _, open := <-ch; open {
		t.Fatalf("expected streamChan to be closed on sentinel return")
	}
}

func TestCanContinue_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		msgs []history.Message
		want bool
	}{
		{"empty", nil, false},
		{"system-only", []history.Message{{Role: "system"}}, false},
		{"clean-final-assistant", []history.Message{{Role: "assistant", Content: "done"}}, false},
		{"dangling-tool-call", []history.Message{{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "c1"}}}}, true},
		{"trailing-user", []history.Message{{Role: "system"}, {Role: "user", Content: "hi"}}, true},
		{"trailing-tool-result", []history.Message{{Role: "tool", ToolCallID: "c1", Content: "ok"}}, true},
	}
	for _, tc := range cases {
		if got := canContinue(tc.msgs); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRegenerate_TruncationIsToolPairSafe(t *testing.T) {
	// Seed a history whose last user is followed by a dangling tool-call group
	// (assistant tool_use without matching tool_result). After Regenerate
	// rewinds to before that user, the prior context still contains a
	// tool_use/tool_result pair that must survive intact.
	provider := &scriptProvider{turns: []LLMResult{
		{Content: "ok"},
	}}
	loop, sm := setup(provider)
	ctx := context.Background()

	sm.SetHistory(ctx, "s1", []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "older"},
		{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "c1", Name: "echo"}}},
		{Role: "tool", ToolCallID: "c1", Content: "echo:older"},
		{Role: "assistant", Content: "older answer"},
		{Role: "user", Content: "newer"},
		{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "c2", Name: "echo"}}},
		// c2's tool result is missing — simulates a crash mid-wave.
	})

	ch := make(chan StreamEvent, 16)
	if err := loop.Regenerate(ctx, "s1", ch); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	_ = drainStream(ch)

	// After the rewind + replay the persisted history must keep the older
	// completed tool group [assistant tool_use, tool, assistant final] intact
	// and never contain a dangling tool_use without its tool_result.
	after := sm.GetHistory(ctx, "s1")
	for i, m := range after {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			// Verify every tool_call has a matching tool result later in history.
			for _, tc := range m.ToolCalls {
				matched := false
				for j := i + 1; j < len(after); j++ {
					if after[j].Role == "tool" && after[j].ToolCallID == tc.ID {
						matched = true
						break
					}
					if after[j].Role == "user" || after[j].Role == "assistant" && len(after[j].ToolCalls) > 0 {
						break
					}
				}
				if !matched {
					t.Fatalf("dangling tool_call %q survived Regenerate: %+v", tc.ID, after)
				}
			}
		}
	}
}
