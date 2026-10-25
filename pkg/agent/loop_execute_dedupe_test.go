package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// sideEffectTool counts executions so tests can tell a reused result from a
// second run of the side effect.
type sideEffectTool struct {
	name    string
	confirm bool
	runs    atomic.Int32
}

func (t *sideEffectTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:                 t.name,
		Description:          "counts runs",
		RequiresConfirmation: t.confirm,
		Display:              tools.DefaultDisplay(t.name, "counts runs"),
	}
}

func (t *sideEffectTool) Execute(_ context.Context, args string) (tools.Result, error) {
	t.runs.Add(1)
	return tools.Text("ran:" + args), nil
}

func toolReplies(t *testing.T, sm *history.InMemSessionManager) []history.Message {
	t.Helper()
	msgs, err := sm.History(context.Background(), "s1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var out []history.Message
	for _, m := range msgs {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}

func TestToolWaves_GatedDuplicatesConfirmAndRunOnce(t *testing.T) {
	cases := map[string][]PendingToolCall{
		"distinct IDs": {
			{ID: "c1", Name: "wipe", ArgsJSON: `{"a":1,"b":2}`},
			{ID: "c2", Name: "wipe", ArgsJSON: `{"b":2, "a":1}`},
		},
		// Gemini reuses the tool name as the call ID.
		"shared ID": {
			{ID: "wipe", Name: "wipe", ArgsJSON: `{}`},
			{ID: "wipe", Name: "wipe", ArgsJSON: `{}`},
		},
	}
	for name, calls := range cases {
		t.Run(name, func(t *testing.T) {
			tool := &sideEffectTool{name: "wipe", confirm: true}
			provider := &scriptProvider{turns: []LLMResult{{ToolCalls: calls}, {Content: "done"}}}
			loop, sm := setup(provider, tool)
			var asks atomic.Int32
			loop.ConfirmHITL = func(context.Context, string, string) bool {
				return asks.Add(1) == 1 // a second prompt would be denied
			}

			if _, err := loop.RunIteration(context.Background(), "s1", "wipe it"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n := asks.Load(); n != 1 {
				t.Fatalf("ConfirmHITL called %d times, want 1", n)
			}
			if n := tool.runs.Load(); n != 1 {
				t.Fatalf("tool ran %d times, want 1", n)
			}
			replies := toolReplies(t, sm)
			if len(replies) != len(calls) {
				t.Fatalf("got %d tool replies, want %d", len(replies), len(calls))
			}
			for i, m := range replies {
				if m.IsError || m.ToolCallID != calls[i].ID {
					t.Fatalf("reply %d = %+v, want approved result for %q", i, m, calls[i].ID)
				}
			}
		})
	}
}

func TestToolWaves_DuplicatesNotMergedWhenUngatedOrArgsDiffer(t *testing.T) {
	gated := &sideEffectTool{name: "wipe", confirm: true}
	plain := &sideEffectTool{name: "read"}
	provider := &scriptProvider{turns: []LLMResult{{ToolCalls: []PendingToolCall{
		{ID: "c1", Name: "wipe", ArgsJSON: `{"k":1}`},
		{ID: "c2", Name: "wipe", ArgsJSON: `{"k":2}`},
		{ID: "c3", Name: "read", ArgsJSON: `{}`},
		{ID: "c4", Name: "read", ArgsJSON: `{}`},
	}}, {Content: "done"}}}
	loop, _ := setup(provider, gated, plain)
	var asks atomic.Int32
	loop.ConfirmHITL = func(context.Context, string, string) bool {
		asks.Add(1)
		return true
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if asks.Load() != 2 || gated.runs.Load() != 2 {
		t.Fatalf("gated: asks=%d runs=%d, want 2 and 2", asks.Load(), gated.runs.Load())
	}
	if n := plain.runs.Load(); n != 2 {
		t.Fatalf("ungated tool ran %d times, want 2", n)
	}
}
