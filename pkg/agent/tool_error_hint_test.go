package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func TestDefaultToolErrorHint_IncludesToolAndError(t *testing.T) {
	out := defaultToolErrorHint("search_events", "ERR_INVALID_DATE_FORMAT: expected ISO 8601")
	if !strings.Contains(out, "search_events") {
		t.Fatalf("hint missing tool name: %q", out)
	}
	if !strings.Contains(out, "ERR_INVALID_DATE_FORMAT") {
		t.Fatalf("hint missing underlying error: %q", out)
	}
	if !strings.Contains(out, "retry") && !strings.Contains(out, "Retry") {
		t.Fatalf("hint should steer toward retry: %q", out)
	}
}

func TestFormatToolError_UsesCustomFormatterWhenSet(t *testing.T) {
	al := &AgentLoop{
		ToolErrorHintFormatter: func(tool, msg string) string {
			return "CUSTOM|" + tool + "|" + msg
		},
	}
	got := al.formatToolError("sql_exec", "syntax error near SELECT")
	want := "CUSTOM|sql_exec|syntax error near SELECT"
	if got != want {
		t.Fatalf("custom formatter not applied.\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatToolError_FallsBackToDefault(t *testing.T) {
	al := &AgentLoop{} // no formatter set
	got := al.formatToolError("web_search", "rate limited")
	if !strings.Contains(got, "web_search") || !strings.Contains(got, "rate limited") {
		t.Fatalf("default formatter should carry tool name + error: %q", got)
	}
}

// --- integration: error flows into tool result message ---

type failingTool struct {
	mu       sync.Mutex
	nCalls   int
	failOnce bool
}

func (t *failingTool) Name() string                    { return "doomed" }
func (t *failingTool) Description() string             { return "always fails" }
func (t *failingTool) ParametersSchema() tools.ToolSchema { return tools.ToolSchema{} }
func (t *failingTool) RequiresConfirmation() bool      { return false }
func (t *failingTool) Execute(_ context.Context, _ string) (string, error) {
	t.mu.Lock()
	t.nCalls++
	t.mu.Unlock()
	return "", errors.New("UPSTREAM_503: gateway said no")
}

func TestToolError_WrappedInToolResult(t *testing.T) {
	// Run a full iteration where the tool fails; the tool result message must
	// carry the formatted hint, not a flat "Error: UPSTREAM_503".
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "doomed", ArgsJSON: `{}`}}},
		{Content: "giving up"},
	}}
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	reg.Register(&failingTool{})
	loop := NewAgentLoop(sm, reg, provider)

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	hist := sm.GetHistory(context.Background(), "s1")
	var toolMsg history.Message
	for _, m := range hist {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			toolMsg = m
		}
	}
	if toolMsg.Role == "" {
		t.Fatal("no tool result message written")
	}
	if !toolMsg.IsError {
		t.Fatal("IsError flag missing on failed tool result")
	}
	if !strings.Contains(toolMsg.Content, "[TOOL_ERROR]") {
		t.Fatalf("formatted hint missing structured prefix: %q", toolMsg.Content)
	}
	if !strings.Contains(toolMsg.Content, "doomed") || !strings.Contains(toolMsg.Content, "UPSTREAM_503") {
		t.Fatalf("hint missing tool/error context: %q", toolMsg.Content)
	}
}

func TestToolError_CustomFormatterRoundTrips(t *testing.T) {
	// Verify the custom formatter override actually reaches the tool result.
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "doomed", ArgsJSON: `{}`}}},
		{Content: "done"},
	}}
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	reg.Register(&failingTool{})
	loop := NewAgentLoop(sm, reg, provider)
	loop.ToolErrorHintFormatter = func(tool, msg string) string {
		return "RETRY_WITH_ISO8601: " + tool + " says " + msg
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	hist := sm.GetHistory(context.Background(), "s1")
	var toolMsg history.Message
	for _, m := range hist {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			toolMsg = m
		}
	}
	if !strings.HasPrefix(toolMsg.Content, "RETRY_WITH_ISO8601:") {
		t.Fatalf("custom formatter output missing from tool result: %q", toolMsg.Content)
	}
}
