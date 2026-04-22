package agent

import (
	"context"
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

	msgs := sm.GetHistory(context.Background(), "s1")
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
