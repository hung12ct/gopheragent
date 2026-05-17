package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// recordingTool captures the argsJSON each call received so tests can assert
// substitution happened. Returns a canned JSON result per input.
type recordingTool struct {
	name   string
	result string

	mu   sync.Mutex
	seen []string
}

func (t *recordingTool) Name() string                   { return t.name }
func (t *recordingTool) Description() string            { return "records invocations" }
func (t *recordingTool) ParametersSchema() tools.ToolSchema { return tools.ToolSchema{} }
func (t *recordingTool) RequiresConfirmation() bool     { return false }
func (t *recordingTool) Display() tools.ToolDisplay { return tools.DefaultDisplay(t.Name(), t.Description()) }
func (t *recordingTool) Execute(_ context.Context, args string) (string, error) {
	t.mu.Lock()
	t.seen = append(t.seen, args)
	t.mu.Unlock()
	return t.result, nil
}

func (t *recordingTool) receivedArgs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.seen))
	copy(out, t.seen)
	return out
}

func TestAgentLoop_DependentToolCalls_SubstituteUpstreamOutput(t *testing.T) {
	fetchUser := &recordingTool{
		name:   "fetch_user",
		result: `{"user_id": "abc123", "name": "Jane"}`,
	}
	fetchOrders := &recordingTool{
		name:   "fetch_orders",
		result: `{"orders": 5}`,
	}

	provider := &scriptProvider{turns: []LLMResult{
		// Turn 1: two tool calls, the second references the first's user_id.
		{ToolCalls: []PendingToolCall{
			{ID: "t1", Name: "fetch_user", ArgsJSON: `{"id": 1}`},
			{ID: "t2", Name: "fetch_orders", ArgsJSON: `{"user_id": <output_of:t1.user_id>}`},
		}},
		// Turn 2: final answer.
		{Content: "done"},
	}}

	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	reg.Register(fetchUser)
	reg.Register(fetchOrders)
	loop := NewAgentLoop(sm, reg, provider)

	resp, err := loop.RunIteration(context.Background(), "s1", "get orders for user 1")
	if err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if resp != "done" {
		t.Fatalf("expected final 'done', got %q", resp)
	}

	// t1 must have run with its original args (no refs to substitute).
	userArgs := fetchUser.receivedArgs()
	if len(userArgs) != 1 || userArgs[0] != `{"id": 1}` {
		t.Fatalf("fetch_user received %v, want [{\"id\": 1}]", userArgs)
	}

	// t2 must have run with user_id substituted to "abc123".
	orderArgs := fetchOrders.receivedArgs()
	if len(orderArgs) != 1 {
		t.Fatalf("fetch_orders received %d calls, want 1: %v", len(orderArgs), orderArgs)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(orderArgs[0]), &parsed); err != nil {
		t.Fatalf("fetch_orders args not valid JSON after substitution: %q (%v)", orderArgs[0], err)
	}
	if parsed["user_id"] != "abc123" {
		t.Fatalf("expected substituted user_id=abc123, got %v (full args: %q)", parsed["user_id"], orderArgs[0])
	}
}

func TestAgentLoop_IndependentToolCalls_RunInSameWave(t *testing.T) {
	a := &recordingTool{name: "a", result: `"done-a"`}
	b := &recordingTool{name: "b", result: `"done-b"`}

	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{
			{ID: "t1", Name: "a", ArgsJSON: `{"x": 1}`},
			{ID: "t2", Name: "b", ArgsJSON: `{"y": 2}`},
		}},
		{Content: "ok"},
	}}

	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	reg.Register(a)
	reg.Register(b)
	loop := NewAgentLoop(sm, reg, provider)

	if _, err := loop.RunIteration(context.Background(), "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if len(a.receivedArgs()) != 1 || len(b.receivedArgs()) != 1 {
		t.Fatalf("expected both tools to run once each: a=%v b=%v", a.receivedArgs(), b.receivedArgs())
	}
}

func TestAgentLoop_CyclicToolCalls_FallbackDoesNotDeadlock(t *testing.T) {
	// Cycle: t1 -> t2, t2 -> t1. Scheduler errors; loop falls back to a
	// single-wave run. Neither tool sees valid substituted args (refs remain
	// unresolved), but the loop must not hang and must terminate cleanly.
	a := &recordingTool{name: "a", result: `"a-out"`}
	b := &recordingTool{name: "b", result: `"b-out"`}

	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{
			{ID: "t1", Name: "a", ArgsJSON: `{"x": <output_of:t2>}`},
			{ID: "t2", Name: "b", ArgsJSON: `{"y": <output_of:t1>}`},
		}},
		{Content: "recovered"},
	}}

	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	reg.Register(a)
	reg.Register(b)
	loop := NewAgentLoop(sm, reg, provider)

	resp, err := loop.RunIteration(context.Background(), "s1", "hi")
	if err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if !strings.Contains(resp, "recovered") {
		t.Fatalf("expected loop to recover and return 'recovered', got %q", resp)
	}
}

// TestAgentLoop_SubstitutionFailureSkipsExecution pins the fail-fast
// contract: when a wave's <output_of:...> reference cannot be resolved
// (e.g. dangling ID), the loop synthesizes a tool-error result and does
// not execute the tool with the literal placeholder string.
func TestAgentLoop_SubstitutionFailureSkipsExecution(t *testing.T) {
	a := &recordingTool{name: "a", result: `"never-reached"`}

	provider := &scriptProvider{turns: []LLMResult{
		// Turn 1: single tool call referencing a non-existent ID.
		{ToolCalls: []PendingToolCall{
			{ID: "t1", Name: "a", ArgsJSON: `{"x": <output_of:ghost>}`},
		}},
		{Content: "ack"},
	}}

	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	reg.Register(a)
	loop := NewAgentLoop(sm, reg, provider)

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}

	// Tool must NOT have been called with the unresolved placeholder.
	if got := len(a.receivedArgs()); got != 0 {
		t.Fatalf("tool ran %d times despite substitution failure; expected 0", got)
	}

	// History must contain a tool-error result for t1.
	hist, _ := sm.History(context.Background(), "s1")
	var toolMsg history.Message
	for _, m := range hist {
		if m.Role == "tool" && m.ToolCallID == "t1" {
			toolMsg = m
			break
		}
	}
	if toolMsg.Role == "" {
		t.Fatal("expected synthesized tool result for t1")
	}
	if !toolMsg.IsError {
		t.Fatalf("tool result should be flagged as error: %+v", toolMsg)
	}
	if !strings.Contains(toolMsg.Content, "tool scheduler") {
		t.Fatalf("expected scheduler error message, got %q", toolMsg.Content)
	}
}
