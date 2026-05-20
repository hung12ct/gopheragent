package agent

import (
	"context"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func TestPriceTable_Compute(t *testing.T) {
	pt := PriceTable{
		"claude-sonnet": {InputPerMTokens: 3, OutputPerMTokens: 15},
	}
	usage := TokenUsage{PromptTokens: 1_000_000, CompletionTokens: 500_000, TotalTokens: 1_500_000}
	got := pt.Compute("claude-sonnet", usage)
	// 1M * $3 + 500K * $15 / 1M = $3 + $7.5 = $10.5
	if got != 10.5 {
		t.Fatalf("expected $10.50, got %v", got)
	}
}

func TestPriceTable_UnknownModelIsZero(t *testing.T) {
	pt := PriceTable{"x": {InputPerMTokens: 1}}
	if got := pt.Compute("missing", TokenUsage{PromptTokens: 1_000_000}); got != 0 {
		t.Fatalf("expected 0 for unknown model, got %v", got)
	}
}

func TestPriceTable_NilTableIsZero(t *testing.T) {
	var pt PriceTable
	if got := pt.Compute("anything", TokenUsage{PromptTokens: 100}); got != 0 {
		t.Fatalf("expected 0 for nil table, got %v", got)
	}
}

func TestRunCost_EmittedOnSuccessfulRun(t *testing.T) {
	prov := &usageStampingProvider{usage: TokenUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}}
	sm := history.NewInMemSessionManager("base")
	reg := tools.NewRegistry()
	var seen []StreamEvent
	loop := New(sm, reg, prov,
		WithPriceTable(PriceTable{"test-model": {InputPerMTokens: 10, OutputPerMTokens: 20}}, "test-model"),
		WithOnEvent(func(_ context.Context, _ string, ev StreamEvent) {
			seen = append(seen, ev)
		}),
	)
	if _, err := loop.RunIteration(context.Background(), "s", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	// Confirm RunCostEvent + DoneEvent both fire. Ordering between them
	// is not guaranteed (defer-based emit can land after Done); both
	// arrive before the stream channel closes so SSE consumers see them.
	costIdx, doneIdx := -1, -1
	for i, ev := range seen {
		switch ev.Payload.(type) {
		case RunCostEvent:
			costIdx = i
		case DoneEvent:
			doneIdx = i
		}
	}
	if costIdx == -1 {
		t.Fatal("RunCostEvent not emitted")
	}
	if doneIdx == -1 {
		t.Fatal("DoneEvent not emitted")
	}
	rc := seen[costIdx].Payload.(RunCostEvent)
	if rc.Model != "test-model" {
		t.Fatalf("expected model='test-model', got %q", rc.Model)
	}
	if rc.Usage.TotalTokens != 1500 {
		t.Fatalf("expected Usage.TotalTokens=1500, got %d", rc.Usage.TotalTokens)
	}
	// 1000 * $10 + 500 * $20 / 1M = 0.01 + 0.01 = 0.02
	if rc.USD != 0.02 {
		t.Fatalf("expected USD=0.02, got %v", rc.USD)
	}
}

func TestRunCost_EmittedOnMaxItersExit(t *testing.T) {
	// Provider always requests an unknown tool — agent never reaches
	// handleFinalAnswer; the loop exhausts MaxIters. Cost rollup must
	// still fire so billing telemetry doesn't miss runaway turns.
	prov := &usageToolLoopingProvider{usage: TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}}
	sm := history.NewInMemSessionManager("base")
	reg := tools.NewRegistry()
	var sawCost bool
	var costEv RunCostEvent
	loop := New(sm, reg, prov,
		WithMaxIters(2),
		WithPriceTable(PriceTable{"m": {InputPerMTokens: 10, OutputPerMTokens: 20}}, "m"),
		WithOnEvent(func(_ context.Context, _ string, ev StreamEvent) {
			if p, ok := ev.Payload.(RunCostEvent); ok {
				sawCost = true
				costEv = p
			}
		}),
	)
	_, _ = loop.RunIteration(context.Background(), "s", "hi")
	if !sawCost {
		t.Fatal("RunCostEvent must fire even when Run terminates via MaxIters")
	}
	if costEv.Usage.TotalTokens == 0 {
		t.Fatalf("expected accumulated tokens > 0 across iterations, got %d", costEv.Usage.TotalTokens)
	}
}

func TestRunCost_EmittedOnRegenerate(t *testing.T) {
	// Regenerate uses runLogicLoop indirectly via its own truncation +
	// re-emit path; assert RunCostEvent fires on the regenerated turn
	// just like the original.
	prov := &usageStampingProvider{usage: TokenUsage{PromptTokens: 50, CompletionTokens: 25, TotalTokens: 75}}
	sm := history.NewInMemSessionManager("base")
	reg := tools.NewRegistry()
	var costs []RunCostEvent
	loop := New(sm, reg, prov,
		WithPriceTable(PriceTable{"m": {InputPerMTokens: 1, OutputPerMTokens: 2}}, "m"),
		WithOnEvent(func(_ context.Context, _ string, ev StreamEvent) {
			if p, ok := ev.Payload.(RunCostEvent); ok {
				costs = append(costs, p)
			}
		}),
	)
	if _, err := loop.RunIteration(context.Background(), "s", "first"); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	if len(costs) != 1 {
		t.Fatalf("expected 1 cost after seed turn, got %d", len(costs))
	}
	seq, err := loop.Regenerate(context.Background(), "s")
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	for range seq {
		// drain
	}
	if len(costs) != 2 {
		t.Fatalf("expected cost event on Regenerate run too, got %d total", len(costs))
	}
}

func TestRunCost_NotEmittedWithoutPriceTable(t *testing.T) {
	prov := &usageStampingProvider{usage: TokenUsage{TotalTokens: 100}}
	sm := history.NewInMemSessionManager("base")
	reg := tools.NewRegistry()
	var sawCost bool
	loop := New(sm, reg, prov,
		WithOnEvent(func(_ context.Context, _ string, ev StreamEvent) {
			if _, ok := ev.Payload.(RunCostEvent); ok {
				sawCost = true
			}
		}),
	)
	if _, err := loop.RunIteration(context.Background(), "s", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if sawCost {
		t.Fatal("RunCostEvent must not fire when PriceTable is nil (zero-cost-when-unused)")
	}
}

func TestRunCost_UnknownModelEmitsZeroUSD(t *testing.T) {
	prov := &usageStampingProvider{usage: TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}}
	sm := history.NewInMemSessionManager("base")
	reg := tools.NewRegistry()
	var rc *RunCostEvent
	loop := New(sm, reg, prov,
		WithPriceTable(PriceTable{"some-other-model": {InputPerMTokens: 1}}, "missing-model"),
		WithOnEvent(func(_ context.Context, _ string, ev StreamEvent) {
			if p, ok := ev.Payload.(RunCostEvent); ok {
				rc = &p
			}
		}),
	)
	if _, err := loop.RunIteration(context.Background(), "s", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if rc == nil {
		t.Fatal("RunCostEvent should still fire (USD=0) when model unknown")
	}
	if rc.USD != 0 {
		t.Fatalf("expected USD=0 for unknown model, got %v", rc.USD)
	}
	if rc.Usage.TotalTokens != 150 {
		t.Fatalf("Usage should be populated regardless of price lookup, got %+v", rc.Usage)
	}
}

// usageStampingProvider returns a fixed Content + stamps Usage on the
// result so the agent loop's UsageEvent path fires. Inline rather than
// adding another helper to memory_test.go since this is cost-test-local.
type usageStampingProvider struct {
	usage TokenUsage
}

func (p *usageStampingProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	ch <- Event(ContentEvent{Text: "ok"})
	return LLMResult{Content: "ok", Usage: p.usage}, nil
}

// usageToolLoopingProvider requests an unknown tool on every call so
// the agent loop never reaches handleFinalAnswer — hitting MaxIters
// instead. Stamps Usage on every call so the cost accumulator records
// per-iteration tokens.
type usageToolLoopingProvider struct {
	usage TokenUsage
}

func (p *usageToolLoopingProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- StreamEvent) (LLMResult, error) {
	return LLMResult{
		ToolCalls: []PendingToolCall{{ID: "c1", Name: "unknown_tool", ArgsJSON: `{}`}},
		Usage:     p.usage,
	}, nil
}
