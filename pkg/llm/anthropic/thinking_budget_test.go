package anthropic

import (
	"context"
	"testing"

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
