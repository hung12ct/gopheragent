package openai

import (
	"context"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
)

func TestReasoningEffortFor_OffForNonReasoningModel(t *testing.T) {
	if got := reasoningEffortFor("gpt-4o", 4096); got != "" {
		t.Fatalf("gpt-4o must not carry reasoning_effort, got %q", got)
	}
}

func TestReasoningEffortFor_ZeroBudgetReturnsEmpty(t *testing.T) {
	if got := reasoningEffortFor("o3-mini", 0); got != "" {
		t.Fatalf("zero budget must not set reasoning_effort, got %q", got)
	}
}

func TestReasoningEffortFor_Thresholds(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{1, "low"},
		{2048, "low"},
		{2049, "medium"},
		{8192, "medium"},
		{8193, "high"},
		{100000, "high"},
	}
	for _, c := range cases {
		if got := reasoningEffortFor("o3-mini", c.budget); got != c.want {
			t.Fatalf("budget=%d: want %q, got %q", c.budget, c.want, got)
		}
	}
}

func TestReasoningEffortFor_ReasoningModelPrefixes(t *testing.T) {
	models := []string{"o1", "o1-mini", "o3", "o3-mini", "o4-mini", "gpt-5-reasoning"}
	for _, m := range models {
		if got := reasoningEffortFor(m, 4096); got != "medium" {
			t.Fatalf("model %q: want medium, got %q", m, got)
		}
	}
}

func TestReasoningEffortFor_IgnoresCase(t *testing.T) {
	if got := reasoningEffortFor("O3-Mini", 4096); got != "medium" {
		t.Fatalf("case-insensitive match expected, got %q", got)
	}
}

func TestReasoningEffort_OptionSentVerbatim(t *testing.T) {
	// "none" is what lets reasoning-by-default models accept function tools
	// on Chat Completions; no model allow-list may stand in its way.
	p, err := New("test-key", "gpt-6-luna", WithReasoningEffort("none"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.reasoningEffort(context.Background()); got != "none" {
		t.Fatalf("expected none, got %q", got)
	}
}

func TestReasoningEffort_OptionBeatsThinkingBudget(t *testing.T) {
	p, err := New("test-key", "o3-mini", WithReasoningEffort("none"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := agent.WithThinkingBudget(context.Background(), 4096)
	if got := p.reasoningEffort(ctx); got != "none" {
		t.Fatalf("explicit option must win over the budget mapping, got %q", got)
	}
}

func TestReasoningEffort_UnsetKeepsBudgetMapping(t *testing.T) {
	p, err := New("test-key", "o3-mini")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.reasoningEffort(context.Background()); got != "" {
		t.Fatalf("unset option and no budget must omit the field, got %q", got)
	}
	ctx := agent.WithThinkingBudget(context.Background(), 4096)
	if got := p.reasoningEffort(ctx); got != "medium" {
		t.Fatalf("expected medium from the budget mapping, got %q", got)
	}
}
