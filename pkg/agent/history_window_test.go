package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// chatHistory builds a system message followed by n user/assistant pairs.
func chatHistory(pairs int) []history.Message {
	msgs := []history.Message{{Role: "system", Content: "sys"}}
	for i := range pairs {
		msgs = append(msgs,
			history.Message{Role: "user", Content: fmt.Sprintf("user message %d", i)},
			history.Message{Role: "assistant", Content: fmt.Sprintf("assistant reply %d", i)},
		)
	}
	return msgs
}

func TestHistoryWindowStart_DisabledOrUnderCapDropsNothing(t *testing.T) {
	msgs := chatHistory(3) // 1 system + 6 conversation
	for _, window := range []int{0, -1, 6, 100} {
		if start, sys := historyWindowStart(msgs, window); start != 1 || sys != 1 {
			t.Fatalf("window %d: start=%d sys=%d, want 1/1", window, start, sys)
		}
	}
}

func TestHistoryWindowStart_StepsAndStaysOnUserBoundary(t *testing.T) {
	const window = 8
	prev := -1
	moves := 0
	for pairs := 5; pairs <= 40; pairs++ {
		msgs := chatHistory(pairs)
		start, sys := historyWindowStart(msgs, window)
		if msgs[start].Role != "user" {
			t.Fatalf("pairs %d: window starts at a %s message", pairs, msgs[start].Role)
		}
		if kept := len(msgs) - start; kept < window/2 || kept > window {
			t.Fatalf("pairs %d: kept %d messages, want %d..%d", pairs, kept, window/2, window)
		}
		if sys != 1 {
			t.Fatalf("pairs %d: sys = %d, want 1", pairs, sys)
		}
		if start != prev {
			moves++
			prev = start
		}
	}
	// 72 added messages with a step of window/2 = 4: the cut moves about
	// once per 4 messages, never once per turn-sized increment of 2.
	if moves > 72/4+1 {
		t.Fatalf("window start moved %d times; a stepped cut should move at most %d", moves, 72/4+1)
	}
}

func TestHistoryWindowStart_NeverOrphansToolResults(t *testing.T) {
	msgs := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "q1"},
		{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "c1", Name: "t"}}},
		{Role: "tool", ToolCallID: "c1", Content: "r1"},
		{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "c2", Name: "t"}}},
		{Role: "tool", ToolCallID: "c2", Content: "r2"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	}
	start, _ := historyWindowStart(msgs, 2)
	if start != 7 {
		t.Fatalf("start = %d, want 7 (the last user message)", start)
	}
	// One turn longer than the window, with no earlier user to snap to:
	// the whole history is kept rather than cutting mid-turn.
	if start, sys := historyWindowStart(msgs[:7], 2); start != sys {
		t.Fatalf("mid-turn cut: start = %d, want %d (nothing dropped)", start, sys)
	}
}

func TestDeriveRequestMessages_WindowKeepsSystemAndRebasesRefs(t *testing.T) {
	stored := chatHistory(10)
	// A long tool result after the cut, outside the protected tail.
	stored = append(stored,
		history.Message{Role: "user", Content: "fetch"},
		history.Message{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "c1", Name: "t"}}},
		history.Message{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("x", softTrimThreshold+100)},
		history.Message{Role: "assistant", Content: "done"},
		history.Message{Role: "user", Content: "thanks"},
		history.Message{Role: "assistant", Content: "np"},
	)
	toolIdx := len(stored) - 4
	before := append([]history.Message(nil), stored...)

	d := deriveRequestMessages(stored, requestPolicy{historyWindow: 8})

	if d.messages[0].Role != "system" {
		t.Fatalf("leading system message dropped: %+v", d.messages[0])
	}
	if d.windowDropped == 0 || d.windowStart-d.windowDropped != 1 {
		t.Fatalf("window start=%d dropped=%d, want a cut right after the system message", d.windowStart, d.windowDropped)
	}
	if got := len(d.messages); got != len(stored)-d.windowDropped {
		t.Fatalf("request carries %d messages, want %d", got, len(stored)-d.windowDropped)
	}
	if len(d.changes) != 1 || d.changes[0].Index != toolIdx {
		t.Fatalf("changes = %+v, want one ref at stored index %d", d.changes, toolIdx)
	}
	for i := range before {
		if err := sameMessage(before[i], stored[i]); err != nil {
			t.Fatalf("stored message %d mutated: %v", i, err)
		}
	}
	if err := derivationReproduces(stored, requestPolicy{historyWindow: 8}, d.messages); err != nil {
		t.Fatalf("windowed request does not reproduce: %v", err)
	}
}

func TestEnforceTokenBudget_WindowCutAloneEmitsTrace(t *testing.T) {
	al := &AgentLoop{HistoryWindow: 4}
	ch := make(chan StreamEvent, 4)
	al.enforceTokenBudget(context.Background(), "s", ch, 0, chatHistory(10))

	traces := collectTrace(ch)
	if len(traces) != 1 {
		t.Fatalf("want 1 trace for a window cut, got %d", len(traces))
	}
	tr := traces[0]
	if tr.WindowDropped == 0 || len(tr.Changes) != 0 {
		t.Fatalf("trace = %+v, want a window cut with no rewrites", tr)
	}
	if tr.EstTokensAfter >= tr.EstTokensBefore {
		t.Fatalf("totals should shrink, got before=%d after=%d", tr.EstTokensBefore, tr.EstTokensAfter)
	}
}

func TestHistoryWindow_RequestOnlyStoredHistoryKept(t *testing.T) {
	provider := &capturingProvider{}
	loop, sm := setup(provider)
	loop.HistoryWindow = 6
	seed := chatHistory(20)
	sm.SaveHistory(context.Background(), "s1", seed)

	if _, err := loop.RunIteration(context.Background(), "s1", "latest question"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}

	if len(provider.seenMsgs) != 1 {
		t.Fatalf("want 1 provider call, got %d", len(provider.seenMsgs))
	}
	conv := conversationOf(provider.seenMsgs[0])
	if len(conv) > 6 || conv[0].Role != "user" {
		t.Fatalf("request carries %d conversation messages starting with %s, want <= 6 starting with user", len(conv), conv[0].Role)
	}
	if last := conv[len(conv)-1]; last.Content != "latest question" {
		t.Fatalf("latest user message missing from request, last = %+v", last)
	}
	stored, _ := sm.History(context.Background(), "s1")
	if want := len(seed) + 2; len(stored) != want {
		t.Fatalf("stored history has %d messages, want %d (window must not persist)", len(stored), want)
	}
}
