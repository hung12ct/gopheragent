package openai

import "testing"

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
