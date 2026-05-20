package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func usageEvent(t *testing.T, pt, ct, tt int) StreamEvent {
	t.Helper()
	return Event(UsageEvent{Usage: TokenUsage{PromptTokens: pt, CompletionTokens: ct, TotalTokens: tt}})
}

func TestBudgetTracker_Snapshot_IsDetachedCopy(t *testing.T) {
	bt := NewBudgetTracker(0)
	bt.Handler()(context.Background(), "s1", usageEvent(t, 100, 50, 150))

	snap := bt.Snapshot()
	snap["s1"] = TokenUsage{TotalTokens: 9999}
	snap["new"] = TokenUsage{TotalTokens: 42}

	live := bt.Usage("s1")
	if live.TotalTokens != 150 {
		t.Fatalf("tracker state leaked through snapshot: got %+v", live)
	}
	if bt.Usage("new").TotalTokens != 0 {
		t.Fatalf("snapshot write leaked into tracker")
	}
}

func TestBudgetTracker_HandlerAccumulates(t *testing.T) {
	bt := NewBudgetTracker(0) // track-only, no enforcement
	h := bt.Handler()
	ctx := context.Background()

	h(ctx, "s1", usageEvent(t, 100, 50, 150))
	h(ctx, "s1", usageEvent(t, 10, 5, 15))
	h(ctx, "s2", usageEvent(t, 200, 100, 300))

	if got := bt.Usage("s1"); got.TotalTokens != 165 || got.PromptTokens != 110 || got.CompletionTokens != 55 {
		t.Fatalf("s1 usage: expected 110/55/165, got %+v", got)
	}
	if got := bt.Usage("s2"); got.TotalTokens != 300 {
		t.Fatalf("s2 usage: expected 300, got %+v", got)
	}
	if got := bt.Usage("absent"); got.TotalTokens != 0 {
		t.Fatalf("missing session should return zero usage, got %+v", got)
	}
}

func TestBudgetTracker_HandlerIgnoresOtherEventTypes(t *testing.T) {
	bt := NewBudgetTracker(0)
	h := bt.Handler()
	h(context.Background(), "s", Event(ContentEvent{Text: "hi"}))
	h(context.Background(), "s", Event(ThoughtEvent{Message: `{"total_tokens":999}`}))
	if bt.Usage("s").TotalTokens != 0 {
		t.Fatalf("non-usage events must not accumulate, got %+v", bt.Usage("s"))
	}
}

func TestBudgetTracker_HandlerIgnoresMalformedPayload(t *testing.T) {
	bt := NewBudgetTracker(0)
	h := bt.Handler()
	// A StreamEvent of usage type but with a non-Usage payload (e.g. an
	// ErrorEvent mistakenly tagged as usage) must not accumulate.
	h(context.Background(), "s", StreamEvent{Type: EventTypeUsage, Payload: ErrorEvent{Message: "not usage"}})
	if bt.Usage("s").TotalTokens != 0 {
		t.Fatalf("malformed payload must not crash or accumulate, got %+v", bt.Usage("s"))
	}
}

func TestBudgetTracker_GuardAllowsUnderBudget(t *testing.T) {
	bt := NewBudgetTracker(1000)
	bt.Handler()(context.Background(), "s", usageEvent(t, 400, 200, 600))

	if err := bt.Guard()(context.Background(), "s", 0); err != nil {
		t.Fatalf("expected Guard to allow (600 < 1000), got %v", err)
	}
}

func TestBudgetTracker_GuardDeniesAtOrAboveBudget(t *testing.T) {
	bt := NewBudgetTracker(1000)
	bt.Handler()(context.Background(), "s", usageEvent(t, 600, 400, 1000))

	err := bt.Guard()(context.Background(), "s", 0)
	if err == nil {
		t.Fatal("expected Guard to deny at budget, got nil")
	}
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("expected 'budget exhausted' in error, got: %v", err)
	}
}

func TestBudgetTracker_GuardDisabledWhenBudgetZero(t *testing.T) {
	bt := NewBudgetTracker(0)
	bt.Handler()(context.Background(), "s", usageEvent(t, 1_000_000, 1_000_000, 2_000_000))

	if err := bt.Guard()(context.Background(), "s", 0); err != nil {
		t.Fatalf("Guard with Budget=0 must never deny, got %v", err)
	}
}

func TestBudgetTracker_ResetClearsSession(t *testing.T) {
	bt := NewBudgetTracker(100)
	h := bt.Handler()
	h(context.Background(), "a", usageEvent(t, 50, 50, 100))
	h(context.Background(), "b", usageEvent(t, 50, 50, 100))

	bt.Reset("a")
	if bt.Usage("a").TotalTokens != 0 {
		t.Fatalf("Reset('a') should clear a's usage, got %+v", bt.Usage("a"))
	}
	if bt.Usage("b").TotalTokens != 100 {
		t.Fatalf("Reset('a') must not affect b, got %+v", bt.Usage("b"))
	}

	bt.Reset("")
	if bt.Usage("b").TotalTokens != 0 {
		t.Fatalf("Reset('') should clear all sessions, got %+v", bt.Usage("b"))
	}
}

func TestBudgetTracker_RewindSubtracts(t *testing.T) {
	bt := NewBudgetTracker(0)
	h := bt.Handler()
	h(context.Background(), "s", usageEvent(t, 60, 40, 100))

	bt.Rewind("s", TokenUsage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30})
	got := bt.Usage("s")
	if got.PromptTokens != 40 || got.CompletionTokens != 30 || got.TotalTokens != 70 {
		t.Fatalf("Rewind mismatch: %+v", got)
	}
}

func TestBudgetTracker_RewindFloorsAtZero(t *testing.T) {
	bt := NewBudgetTracker(0)
	h := bt.Handler()
	h(context.Background(), "s", usageEvent(t, 10, 5, 15))

	bt.Rewind("s", TokenUsage{PromptTokens: 100, CompletionTokens: 100, TotalTokens: 200})
	got := bt.Usage("s")
	if got.PromptTokens != 0 || got.CompletionTokens != 0 || got.TotalTokens != 0 {
		t.Fatalf("Rewind should floor at zero, got %+v", got)
	}
}

func TestBudgetTracker_RewindUnknownSessionNoop(t *testing.T) {
	bt := NewBudgetTracker(0)
	// Should not panic, should not create a phantom entry.
	bt.Rewind("never-seen", TokenUsage{TotalTokens: 50})
	if got := bt.Usage("never-seen"); got.TotalTokens != 0 {
		t.Fatalf("expected unknown session to stay empty, got %+v", got)
	}
}

func TestBudgetTracker_RewindEmptyKeyNoop(t *testing.T) {
	bt := NewBudgetTracker(0)
	h := bt.Handler()
	h(context.Background(), "real", usageEvent(t, 10, 10, 20))
	// Empty key must NOT iterate every session; this would be a surprise.
	bt.Rewind("", TokenUsage{TotalTokens: 99})
	if got := bt.Usage("real").TotalTokens; got != 20 {
		t.Fatalf("Rewind('') should be a no-op (not clear all), got %d", got)
	}
}

func TestBudgetTracker_RewindReleasesBudget(t *testing.T) {
	// Real flow: budget exhausted, Rewind frees room for another call.
	bt := NewBudgetTracker(100)
	h := bt.Handler()
	g := bt.Guard()
	h(context.Background(), "s", usageEvent(t, 50, 50, 100))
	if err := g(context.Background(), "s", 0); err == nil {
		t.Fatal("expected guard to deny at-budget")
	}
	bt.Rewind("s", TokenUsage{TotalTokens: 30})
	if err := g(context.Background(), "s", 0); err != nil {
		t.Fatalf("guard should allow after Rewind: %v", err)
	}
}

func TestBudgetTracker_ConcurrentAccess(t *testing.T) {
	bt := NewBudgetTracker(0)
	h := bt.Handler()
	g := bt.Guard()

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(3)
		go func() { defer wg.Done(); h(context.Background(), "s", usageEvent(t, 1, 1, 2)) }()
		go func() { defer wg.Done(); _ = g(context.Background(), "s", 0) }()
		go func() { defer wg.Done(); _ = bt.Usage("s") }()
	}
	wg.Wait()

	// 50 accumulate calls of 2 total = 100.
	if got := bt.Usage("s").TotalTokens; got != 100 {
		t.Fatalf("expected 100 after 50 concurrent adds, got %d", got)
	}
}
