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

func TestRunCost_EmittedBeforeDone(t *testing.T) {
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
	// Find RunCostEvent + DoneEvent and verify order.
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
	if costIdx >= doneIdx {
		t.Fatalf("RunCostEvent must precede DoneEvent (cost=%d, done=%d)", costIdx, doneIdx)
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
