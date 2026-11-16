package anthropic

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/hung12ct/gopheragent/pkg/agent"
)

func TestResolveThinkingBudget_ZeroWhenUnset(t *testing.T) {
	if got := resolveThinkingBudget(context.Background(), 4096); got != 0 {
		t.Fatalf("no budget on ctx should return 0, got %d", got)
	}
}

func TestResolveThinkingBudget_RaisesToFloor(t *testing.T) {
	ctx := agent.WithThinkingBudget(context.Background(), 200)
	got := resolveThinkingBudget(ctx, 8192)
	if got != 1024 {
		t.Fatalf("budget below floor should clamp to 1024, got %d", got)
	}
}

func TestResolveThinkingBudget_AtOrAboveMaxTokensClamps(t *testing.T) {
	ctx := agent.WithThinkingBudget(context.Background(), 4096)
	got := resolveThinkingBudget(ctx, 4096)
	if got != 4095 {
		t.Fatalf("budget must be < maxTokens, want 4095, got %d", got)
	}
}

func TestResolveThinkingBudget_DisabledWhenMaxTokensTooSmall(t *testing.T) {
	ctx := agent.WithThinkingBudget(context.Background(), 4096)
	// MaxTokens == 1024 (floor) — no room for any output, must disable.
	if got := resolveThinkingBudget(ctx, 1024); got != 0 {
		t.Fatalf("tiny max_tokens should disable thinking, got %d", got)
	}
}

func TestResolveThinkingBudget_HappyPath(t *testing.T) {
	ctx := agent.WithThinkingBudget(context.Background(), 4096)
	if got := resolveThinkingBudget(ctx, 8192); got != 4096 {
		t.Fatalf("in-range budget should pass through, got %d", got)
	}
}

func thinkingParams(t *testing.T, model string, budget int, opts ...Option) (anthropic.MessageNewParams, bool) {
	t.Helper()
	p, err := New("test-key", model, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var params anthropic.MessageNewParams
	on := p.applyThinking(agent.WithThinkingBudget(context.Background(), budget), &params)
	return params, on
}

func TestApplyThinking_CurrentModelUsesAdaptiveAndEffort(t *testing.T) {
	// Current models answer a token budget with a 400; the budget must
	// become adaptive thinking plus an effort level instead.
	for budget, want := range map[int]string{1024: "low", 4096: "medium", 32000: "high"} {
		params, on := thinkingParams(t, "claude-opus-5-5", budget)
		if !on || params.Thinking.OfAdaptive == nil || params.Thinking.OfEnabled != nil {
			t.Fatalf("budget %d: want adaptive thinking, got %+v", budget, params.Thinking)
		}
		if got := string(params.OutputConfig.Effort); got != want {
			t.Fatalf("budget %d: want effort %q, got %q", budget, want, got)
		}
	}
}

func TestApplyThinking_UnknownModelIsTreatedAsCurrent(t *testing.T) {
	params, _ := thinkingParams(t, "claude-some-future-model", 4096)
	if params.Thinking.OfAdaptive == nil {
		t.Fatalf("a model outside the legacy set must get adaptive thinking, got %+v", params.Thinking)
	}
}

func TestApplyThinking_LegacyModelKeepsTokenBudget(t *testing.T) {
	for _, model := range []string{"claude-haiku-4-5", "claude-sonnet-4-5-20250929", "claude-opus-4-1"} {
		params, on := thinkingParams(t, model, 4096)
		if !on || params.Thinking.OfEnabled == nil || params.Thinking.OfEnabled.BudgetTokens != 4096 {
			t.Fatalf("%s: want enabled thinking with the budget, got %+v", model, params.Thinking)
		}
		if params.OutputConfig.Effort != "" {
			t.Fatalf("%s: a budget must not invent an effort, got %q", model, params.OutputConfig.Effort)
		}
	}
}

func TestApplyThinking_WithEffortWinsAndWorksWithoutBudget(t *testing.T) {
	params, on := thinkingParams(t, "claude-sonnet-5-5", 32000, WithEffort("xhigh"))
	if !on || params.OutputConfig.Effort != "xhigh" {
		t.Fatalf("explicit effort must win over the budget mapping, got on=%v effort=%q", on, params.OutputConfig.Effort)
	}
	params, on = thinkingParams(t, "claude-sonnet-5-5", 0, WithEffort("low"))
	if on || params.OutputConfig.Effort != "low" {
		t.Fatalf("effort must be sent without a budget, got on=%v effort=%q", on, params.OutputConfig.Effort)
	}
	if params.Thinking.OfAdaptive != nil || params.Thinking.OfEnabled != nil {
		t.Fatalf("no budget must leave thinking unset, got %+v", params.Thinking)
	}
}

func TestApplyThinking_UnsetSendsNothing(t *testing.T) {
	params, on := thinkingParams(t, "claude-sonnet-5-5", 0)
	if on || params.OutputConfig.Effort != "" || params.Thinking.OfAdaptive != nil {
		t.Fatalf("no budget and no effort must not touch the request, got %+v", params)
	}
}
