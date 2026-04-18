package agent

import (
	"context"
	"testing"
)

func TestThinkingBudget_EmptyCtxReturnsZero(t *testing.T) {
	if got := ThinkingBudgetFromContext(context.Background()); got != 0 {
		t.Fatalf("empty ctx: want 0, got %d", got)
	}
}

func TestThinkingBudget_RoundTrip(t *testing.T) {
	ctx := WithThinkingBudget(context.Background(), 4096)
	if got := ThinkingBudgetFromContext(ctx); got != 4096 {
		t.Fatalf("round trip: want 4096, got %d", got)
	}
}

func TestThinkingBudget_NonPositiveClearsHint(t *testing.T) {
	cases := []int{0, -1, -999}
	for _, n := range cases {
		ctx := WithThinkingBudget(context.Background(), n)
		if got := ThinkingBudgetFromContext(ctx); got != 0 {
			t.Fatalf("WithThinkingBudget(%d): want 0, got %d", n, got)
		}
	}
}

func TestThinkingBudget_OverwritesEarlierValue(t *testing.T) {
	ctx := WithThinkingBudget(context.Background(), 1024)
	ctx = WithThinkingBudget(ctx, 8192)
	if got := ThinkingBudgetFromContext(ctx); got != 8192 {
		t.Fatalf("want latest value 8192, got %d", got)
	}
}

func TestThinkingBudget_WrongTypeValueIgnored(t *testing.T) {
	// A stray value at a different key must not leak into the helper.
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, 4096)
	if got := ThinkingBudgetFromContext(ctx); got != 0 {
		t.Fatalf("unrelated ctx value must not leak: got %d", got)
	}
}
