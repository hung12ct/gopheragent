package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOnToolResult_RewritesSuccessResult(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(1)},
		{Content: "final"},
	}}
	loop, sm := setup(provider, ct)

	loop.OnToolResult = func(_ context.Context, name, _, result string) (string, error) {
		if name != "counter" {
			t.Errorf("hook saw unexpected tool name: %q", name)
		}
		return strings.ReplaceAll(result, "ok:", "rewritten:"), nil
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := sm.GetHistory(context.Background(), "s1")
	var seenRewritten bool
	for _, m := range msgs {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "rewritten:") {
			seenRewritten = true
		}
	}
	if !seenRewritten {
		t.Fatalf("rewritten tool result never reached the LLM context — msgs: %+v", msgs)
	}
}

func TestOnToolResult_VetoConvertsToToolError(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(1)},
		{Content: "final"},
	}}
	loop, sm := setup(provider, ct)

	veto := errors.New("post-validation rejected the output")
	loop.OnToolResult = func(_ context.Context, _, _, _ string) (string, error) {
		return "", veto
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := sm.GetHistory(context.Background(), "s1")
	var sawError bool
	for _, m := range msgs {
		if m.Role == "tool" && m.IsError && strings.Contains(m.Content, "post-validation rejected") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("hook veto never produced a tool-error message in history — msgs: %+v", msgs)
	}
}

func TestOnToolResult_NilHookIsZeroCost(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(1)},
		{Content: "final"},
	}}
	loop, sm := setup(provider, ct)
	// loop.OnToolResult left nil

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := sm.GetHistory(context.Background(), "s1")
	var sawOk bool
	for _, m := range msgs {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "ok:") {
			sawOk = true
		}
	}
	if !sawOk {
		t.Fatalf("expected raw tool result through when OnToolResult is nil — msgs: %+v", msgs)
	}
}
