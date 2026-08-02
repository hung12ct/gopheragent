package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// collectTrace drains ch and returns every ContextTraceEvent payload seen.
func collectTrace(ch chan StreamEvent) []ContextTraceEvent {
	close(ch)
	var out []ContextTraceEvent
	for ev := range ch {
		if p, ok := ev.Payload.(ContextTraceEvent); ok {
			out = append(out, p)
		}
	}
	return out
}

func TestEnforceTokenBudget_UnbudgetedPathEmitsTrace(t *testing.T) {
	al := &AgentLoop{}
	ch := make(chan StreamEvent, 16)
	msgs := []history.Message{
		{Role: "tool", Content: strings.Repeat("x", softTrimThreshold+100), ToolCallID: "call-1", CorrelationID: "corr-1"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "again"},
	}

	al.enforceTokenBudget(context.Background(), "s", ch, 2, msgs)

	traces := collectTrace(ch)
	if len(traces) != 1 {
		t.Fatalf("want 1 trace on the unbudgeted path, got %d", len(traces))
	}
	tr := traces[0]
	if tr.Policy != ContextPolicyDefault {
		t.Fatalf("policy = %q, want %q", tr.Policy, ContextPolicyDefault)
	}
	if tr.Iteration != 2 {
		t.Fatalf("iteration = %d, want 2", tr.Iteration)
	}
	if len(tr.Changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(tr.Changes))
	}
	c := tr.Changes[0]
	if c.Index != 0 || c.Role != "tool" || c.Reason != ContextChangeSoftTrim {
		t.Fatalf("change = %+v, want index 0 / tool / soft-trim", c)
	}
	if c.ToolCallID != "call-1" || c.CorrelationID != "corr-1" {
		t.Fatalf("change lost its correlation handles: %+v", c)
	}
	if c.EstTokensAfter >= c.EstTokensBefore {
		t.Fatalf("est tokens should shrink, got before=%d after=%d", c.EstTokensBefore, c.EstTokensAfter)
	}
	if tr.EstTokensAfter >= tr.EstTokensBefore {
		t.Fatalf("totals should shrink, got before=%d after=%d", tr.EstTokensBefore, tr.EstTokensAfter)
	}
}

func TestEnforceTokenBudget_NoChangeEmitsNothing(t *testing.T) {
	al := &AgentLoop{}
	ch := make(chan StreamEvent, 16)
	msgs := []history.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "short"},
		{Role: "user", Content: "again"},
	}

	al.enforceTokenBudget(context.Background(), "s", ch, 0, msgs)

	if traces := collectTrace(ch); len(traces) != 0 {
		t.Fatalf("want no trace when nothing was pruned, got %d", len(traces))
	}
}

func TestEnforceTokenBudget_EmergencyPolicyTagged(t *testing.T) {
	// MaxTokenBudget=1 puts the estimate past the ceiling outright, which
	// skips the warn window (it only covers thresh < est <= budget) and
	// goes straight to the shallow emergency prune.
	al := &AgentLoop{MaxTokenBudget: 1}
	ch := make(chan StreamEvent, 16)
	msgs := []history.Message{
		{Role: "tool", Content: strings.Repeat("y", softTrimThreshold+100), ToolCallID: "call-1"},
		{Role: "user", Content: "hi"},
	}

	al.enforceTokenBudget(context.Background(), "s", ch, 0, msgs)

	traces := collectTrace(ch)
	if len(traces) != 1 {
		t.Fatalf("want 1 trace, got %d", len(traces))
	}
	if traces[0].Policy != ContextPolicyBudgetEmergency {
		t.Fatalf("policy = %q, want %q", traces[0].Policy, ContextPolicyBudgetEmergency)
	}
	if len(traces[0].Changes) != 1 || traces[0].Changes[0].Reason != ContextChangeSoftTrim {
		t.Fatalf("changes = %+v, want a single soft-trim", traces[0].Changes)
	}
}

func TestEnforceTokenBudget_WarnPolicyTagsArgsTruncation(t *testing.T) {
	// Size the budget so the estimate lands inside the warn window:
	// thresh (0.85 * 1200 = 1020) < est (~1100) <= 1200.
	al := &AgentLoop{MaxTokenBudget: 1200}
	ch := make(chan StreamEvent, 16)
	msgs := []history.Message{
		{Role: "tool", Content: strings.Repeat("y", 4400), ToolCallID: "call-1", CorrelationID: "corr-1"},
		{Role: "user", Content: "hi"},
	}

	al.enforceTokenBudget(context.Background(), "s", ch, 0, msgs)

	traces := collectTrace(ch)
	if len(traces) != 1 {
		t.Fatalf("want 1 trace, got %d", len(traces))
	}
	if traces[0].Policy != ContextPolicyBudgetWarn {
		t.Fatalf("policy = %q, want %q", traces[0].Policy, ContextPolicyBudgetWarn)
	}
	if len(traces[0].Changes) != 1 {
		t.Fatalf("changes = %+v, want exactly one", traces[0].Changes)
	}
	c := traces[0].Changes[0]
	if c.Reason != ContextChangeArgsTruncated || c.CorrelationID != "corr-1" {
		t.Fatalf("change = %+v, want args-truncated on corr-1", c)
	}
}

func TestPruneContextMessages_OutlierReasonRecorded(t *testing.T) {
	msgs := []history.Message{
		{Role: "tool", Content: strings.Repeat("z", outlierTrimThreshold+10)},
		{Role: "user", Content: "hi"},
	}

	_, changes := pruneContextMessages(msgs, 1)

	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(changes))
	}
	if changes[0].Reason != ContextChangeOutlierDiscarded {
		t.Fatalf("reason = %q, want %q", changes[0].Reason, ContextChangeOutlierDiscarded)
	}
}

func TestPruneContextMessages_NoChangeAllocatesNoTrace(t *testing.T) {
	msgs := []history.Message{
		{Role: "tool", Content: "small"},
		{Role: "user", Content: "hi"},
	}

	if _, changes := pruneContextMessages(msgs, 1); changes != nil {
		t.Fatalf("want nil trace when nothing changed, got %+v", changes)
	}
}
