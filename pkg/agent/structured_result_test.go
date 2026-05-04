package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// SQLPayload is a sample structured payload — the kind of typed shape
// adopters would attach to a tool that returns rich data alongside its
// LLM-facing string.
type SQLPayload struct {
	RowCount int
	Columns  []string
}

// structuredCounterTool implements both Tool (via Execute) and
// tools.StructuredResult (via ExecuteStructured). The agent loop must
// prefer the structured path so the typed payload reaches OnToolResult.
type structuredCounterTool struct {
	name string
}

func (s *structuredCounterTool) Name() string                       { return s.name }
func (s *structuredCounterTool) Description() string                { return "structured counter" }
func (s *structuredCounterTool) ParametersSchema() tools.ToolSchema { return tools.ToolSchema{} }
func (s *structuredCounterTool) RequiresConfirmation() bool         { return false }
func (s *structuredCounterTool) Display() tools.ToolDisplay {
	return tools.DefaultDisplay(s.name, "structured counter")
}
func (s *structuredCounterTool) Execute(ctx context.Context, args string) (string, error) {
	r, _, err := s.ExecuteStructured(ctx, args)
	return r, err
}
func (s *structuredCounterTool) ExecuteStructured(_ context.Context, _ string) (string, any, error) {
	return "table rendered as markdown", SQLPayload{RowCount: 42, Columns: []string{"id", "name"}}, nil
}

func TestStructuredResult_PayloadReachesHook(t *testing.T) {
	tool := &structuredCounterTool{name: "structured"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "structured", ArgsJSON: "{}"}}},
		{Content: "final"},
	}}
	loop, _ := setup(provider, tool)

	var seen any
	var mu sync.Mutex
	loop.OnToolResult = func(_ context.Context, _, _, _, result string, structured any, _ error) (string, error) {
		mu.Lock()
		seen = structured
		mu.Unlock()
		return result, nil
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	p, ok := seen.(SQLPayload)
	if !ok {
		t.Fatalf("hook did not receive SQLPayload — got %T (%+v)", seen, seen)
	}
	if p.RowCount != 42 || len(p.Columns) != 2 {
		t.Fatalf("structured payload did not round-trip: %+v", p)
	}
}

func TestStructuredResult_NonStructuredToolDeliversNilPayload(t *testing.T) {
	// Tools that don't implement StructuredResult should still go through
	// the hook with structured == nil so adopters can branch on
	// presence-of-payload without surprise.
	tool := &countingTool{name: "plain"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "plain", ArgsJSON: "{}"}}},
		{Content: "final"},
	}}
	loop, _ := setup(provider, tool)

	var sawNil bool
	loop.OnToolResult = func(_ context.Context, _, _, _, result string, structured any, _ error) (string, error) {
		if structured == nil {
			sawNil = true
		}
		return result, nil
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sawNil {
		t.Fatal("hook should receive nil structured payload for non-StructuredResult tools")
	}
}

func TestStructuredResult_HookCanRewriteUsingTypedFields(t *testing.T) {
	// Realistic flow: hook reads typed Columns from structured payload and
	// rewrites the LLM-facing result accordingly. Demonstrates the value
	// of pairing OnToolResult with StructuredResult.
	tool := &structuredCounterTool{name: "structured"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "structured", ArgsJSON: "{}"}}},
		{Content: "final"},
	}}
	loop, sm := setup(provider, tool)

	loop.OnToolResult = func(_ context.Context, _, _, _, result string, structured any, _ error) (string, error) {
		if p, ok := structured.(SQLPayload); ok {
			return result + "\n\n[" + strings.Join(p.Columns, ",") + "]", nil
		}
		return result, nil
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := sm.GetHistory(context.Background(), "s1")
	var found bool
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, "[id,name]") {
			found = true
		}
	}
	if !found {
		t.Fatalf("typed-field rewrite never reached history: %+v", msgs)
	}
}
