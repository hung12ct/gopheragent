package agent

import (
	"context"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// inlineRenderTool implements tools.Tool plus tools.InlineRenderer with
// InlineResult()=true — same shape as builtin/generate_image.
type inlineRenderTool struct{}

func (inlineRenderTool) Name() string                       { return "render" }
func (inlineRenderTool) Description() string                { return "renders inline" }
func (inlineRenderTool) ParametersSchema() tools.ToolSchema { return tools.ToolSchema{} }
func (inlineRenderTool) RequiresConfirmation() bool         { return false }
func (inlineRenderTool) Display() tools.ToolDisplay         { return tools.DefaultDisplay("render", "renders inline") }
func (inlineRenderTool) Execute(_ context.Context, _ string) (string, error) {
	return "![cat](https://example.test/cat.png)", nil
}
func (inlineRenderTool) InlineResult() bool { return true }

// plainTool implements only tools.Tool; its results are NOT inline.
type plainTool struct{}

func (plainTool) Name() string                       { return "plain" }
func (plainTool) Description() string                { return "plain tool" }
func (plainTool) ParametersSchema() tools.ToolSchema { return tools.ToolSchema{} }
func (plainTool) RequiresConfirmation() bool         { return false }
func (plainTool) Display() tools.ToolDisplay         { return tools.DefaultDisplay("plain", "plain tool") }
func (plainTool) Execute(_ context.Context, _ string) (string, error) {
	return "ok", nil
}

func TestInlineResultTool_PersistedRowCarriesIsInlineResultFlag(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "render", ArgsJSON: "{}"}}},
		{Content: "final"},
	}}
	loop, sm := setup(provider, inlineRenderTool{})

	if _, err := loop.RunIteration(context.Background(), "s1", "make me a cat"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stored := sm.GetHistory(context.Background(), "s1")
	var found bool
	for _, m := range stored {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			if !m.IsInlineResult {
				t.Fatalf("inline-render tool row missing IsInlineResult flag: %+v", m)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no tool row recorded for inline-render tool — stored=%+v", stored)
	}
}

func TestPlainTool_PersistedRowDoesNotCarryIsInlineResultFlag(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "plain", ArgsJSON: "{}"}}},
		{Content: "final"},
	}}
	loop, sm := setup(provider, plainTool{})

	if _, err := loop.RunIteration(context.Background(), "s1", "do something"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stored := sm.GetHistory(context.Background(), "s1")
	for _, m := range stored {
		if m.Role == "tool" && m.IsInlineResult {
			t.Fatalf("plain tool row should not have IsInlineResult set: %+v", m)
		}
	}
}

func TestInlineResultTool_FailingExecuteDoesNotSetFlag(t *testing.T) {
	// On error the tool result is the formatted error message, not the
	// inline content — the flag must stay false so adopters don't fold
	// errors into the assistant body.
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "render", ArgsJSON: "{}"}}},
		{Content: "final"},
	}}
	loop, sm := setup(provider, failingInlineRenderTool{})

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stored := sm.GetHistory(context.Background(), "s1")
	for _, m := range stored {
		if m.Role == "tool" && m.IsInlineResult {
			t.Fatalf("failing inline tool must not be flagged inline: %+v", m)
		}
	}
}

type failingInlineRenderTool struct{}

func (failingInlineRenderTool) Name() string                       { return "render" }
func (failingInlineRenderTool) Description() string                { return "renders inline" }
func (failingInlineRenderTool) ParametersSchema() tools.ToolSchema { return tools.ToolSchema{} }
func (failingInlineRenderTool) RequiresConfirmation() bool         { return false }
func (failingInlineRenderTool) Display() tools.ToolDisplay         { return tools.DefaultDisplay("render", "renders inline") }
func (failingInlineRenderTool) Execute(_ context.Context, _ string) (string, error) {
	return "", &mockToolErr{msg: "render failed"}
}
func (failingInlineRenderTool) InlineResult() bool { return true }

type mockToolErr struct{ msg string }

func (e *mockToolErr) Error() string { return e.msg }

// Ensure the history import is used (also keeps go imports tidy on this file).
var _ = history.Message{}
