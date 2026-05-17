package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// countingTool tallies Execute invocations so budget tests can assert how
// many of the model's requested calls actually ran.
type countingTool struct {
	name  string
	calls atomic.Int32
}

func (c *countingTool) Name() string                       { return c.name }
func (c *countingTool) Description() string                { return "counts calls" }
func (c *countingTool) ParametersSchema() tools.ToolSchema { return tools.ToolSchema{} }
func (c *countingTool) RequiresConfirmation() bool         { return false }
func (c *countingTool) Display() tools.ToolDisplay { return tools.DefaultDisplay(c.Name(), c.Description()) }
func (c *countingTool) Execute(_ context.Context, args string) (string, error) {
	c.calls.Add(1)
	return "ok:" + args, nil
}

func fanoutCalls(n int) []PendingToolCall {
	out := make([]PendingToolCall, n)
	for i := 0; i < n; i++ {
		out[i] = PendingToolCall{
			ID:       "c" + string(rune('0'+i)),
			Name:     "counter",
			ArgsJSON: `{"i":` + string(rune('0'+i)) + `}`,
		}
	}
	return out
}

func TestMaxToolCallsPerTurn_Unlimited(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(5)},
		{Content: "final"},
	}}
	loop, _ := setup(provider, ct)
	// MaxToolCallsPerTurn default 0 = unlimited

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := ct.calls.Load(); n != 5 {
		t.Fatalf("expected 5 executions, got %d", n)
	}
}

func TestMaxToolCallsPerTurn_Truncates(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(5)},
		{Content: "final"},
	}}
	loop, sm := setup(provider, ct)
	loop.MaxToolCallsPerTurn = 2

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if n := ct.calls.Load(); n != 2 {
		t.Fatalf("expected 2 executions, got %d", n)
	}

	msgs, _ := sm.History(context.Background(), "s1")
	dropped := 0
	for _, m := range msgs {
		if m.Role == "tool" && m.IsError && strings.Contains(m.Content, "dropped by per-turn tool-call budget") {
			dropped++
		}
	}
	if dropped != 3 {
		t.Fatalf("expected 3 synthesized drop messages in history, got %d", dropped)
	}
}

func TestMaxToolCallsPerTurn_EmitsThought(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(5)},
		{Content: "final"},
	}}
	loop, _ := setup(provider, ct)
	loop.MaxToolCallsPerTurn = 2

	var thoughtHits int
	loop.OnEvent(func(_ context.Context, _ string, ev StreamEvent) {
		if ev.Type == "thought" && strings.Contains(ev.Content, "Tool-call budget exceeded") {
			thoughtHits++
		}
	})

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if thoughtHits != 1 {
		t.Fatalf("expected exactly 1 budget-exceeded thought event, got %d", thoughtHits)
	}
}

func TestMaxToolCallsPerTurn_ExactMatch(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(3)},
		{Content: "final"},
	}}
	loop, _ := setup(provider, ct)
	loop.MaxToolCallsPerTurn = 3

	var thoughtHits int
	loop.OnEvent(func(_ context.Context, _ string, ev StreamEvent) {
		if ev.Type == "thought" && strings.Contains(ev.Content, "Tool-call budget exceeded") {
			thoughtHits++
		}
	})

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := ct.calls.Load(); n != 3 {
		t.Fatalf("expected 3 executions, got %d", n)
	}
	if thoughtHits != 0 {
		t.Fatalf("expected no budget event for exact-match, got %d", thoughtHits)
	}
}

func TestMaxToolCallsPerSession_AbortsAcrossIterations(t *testing.T) {
	// Three iterations, two tool calls each. Cap is 3 — execution must
	// abort after iteration 2 (cumulative=4 ≥ 3) before iteration 3 runs.
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(2)},
		{ToolCalls: fanoutCalls(2)},
		{ToolCalls: fanoutCalls(2)}, // should never run
		{Content: "final"},
	}}
	loop, _ := setup(provider, ct)
	loop.MaxToolCallsPerSession = 3

	_, err := loop.RunIteration(context.Background(), "s1", "go")
	if err == nil {
		t.Fatalf("expected ErrMaxToolCallsPerSession, got nil")
	}
	if !errors.Is(err, ErrMaxToolCallsPerSession) {
		t.Fatalf("expected ErrMaxToolCallsPerSession, got %v", err)
	}
	if n := ct.calls.Load(); n != 4 {
		t.Fatalf("expected 4 executions before abort, got %d", n)
	}
}

func TestMaxToolCallsPerSession_ZeroIsUnlimited(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(2)},
		{ToolCalls: fanoutCalls(2)},
		{ToolCalls: fanoutCalls(2)},
		{Content: "final"},
	}}
	loop, _ := setup(provider, ct)
	// MaxToolCallsPerSession default 0 = unlimited

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := ct.calls.Load(); n != 6 {
		t.Fatalf("expected all 6 executions, got %d", n)
	}
}
