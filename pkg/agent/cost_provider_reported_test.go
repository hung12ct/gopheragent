package agent

import (
	"context"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// noopCostTool lets a multi-call Run finish cleanly instead of erroring on an
// unregistered tool, so the cost assertions are not entangled with failure
// handling.
type noopCostTool struct{}

func (noopCostTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        "noop",
		Description: "does nothing",
		Display:     tools.DefaultDisplay("noop", "does nothing"),
	}
}

func (noopCostTool) Execute(_ context.Context, _ string) (tools.Result, error) {
	return tools.Text("ok"), nil
}

// closeUSD compares dollar amounts with a tolerance well below a cent.
// CostUSD is a float64, so summing several calls carries the usual binary
// drift (0.05 + 0.02 need not be exactly 0.07); exact equality here would
// make the tests brittle without saying anything about correctness.
func closeUSD(got, want float64) bool {
	d := got - want
	return d < 1e-9 && d > -1e-9
}

// runForCost drives one Run and returns the RunCostEvent it emitted, if any.
func runForCost(t *testing.T, prov LLMProvider, opts ...Option) (RunCostEvent, bool) {
	t.Helper()
	var got RunCostEvent
	var seen bool
	opts = append(opts, WithOnEvent(func(_ context.Context, _ string, ev StreamEvent) {
		if p, ok := ev.Payload.(RunCostEvent); ok {
			got, seen = p, true
		}
	}))
	reg := tools.NewRegistry()
	reg.Register(noopCostTool{})
	loop := New(history.NewInMemSessionManager("base"), reg, prov, opts...)
	if _, err := loop.RunIteration(context.Background(), "s", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	return got, seen
}

// The headline case: a gateway that bills per request needs no PriceTable.
// Gating the accumulator on the table used to drop this figure entirely.
func TestRunCost_ProviderReportedCostNeedsNoPriceTable(t *testing.T) {
	prov := &usageStampingProvider{usage: TokenUsage{
		PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, CostUSD: 0.0731,
	}}

	rc, seen := runForCost(t, prov)
	if !seen {
		t.Fatal("RunCostEvent not emitted for a provider-priced Run without a PriceTable")
	}
	if !closeUSD(rc.USD, 0.0731) {
		t.Fatalf("USD = %v, want the provider-reported 0.0731", rc.USD)
	}
	if !closeUSD(rc.Usage.CostUSD, 0.0731) {
		t.Fatalf("Usage.CostUSD = %v, want 0.0731 so adopters can tell exact from estimated", rc.Usage.CostUSD)
	}
	if rc.Usage.TotalTokens != 1500 {
		t.Fatalf("Usage.TotalTokens = %d, want 1500", rc.Usage.TotalTokens)
	}
}

// A reported charge is the real bill and must beat the table estimate, which
// cannot know which model a gateway routed to.
func TestRunCost_ProviderReportedCostBeatsPriceTable(t *testing.T) {
	prov := &usageStampingProvider{usage: TokenUsage{
		PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, CostUSD: 0.0731,
	}}

	// The table would compute 1000*$10 + 500*$20 per 1M = 0.02.
	rc, seen := runForCost(t, prov,
		WithPriceTable(PriceTable{"m": {InputPerMTokens: 10, OutputPerMTokens: 20}}, "m"))
	if !seen {
		t.Fatal("RunCostEvent not emitted")
	}
	if !closeUSD(rc.USD, 0.0731) {
		t.Fatalf("USD = %v, want the provider figure 0.0731 to win over the 0.02 estimate", rc.USD)
	}
}

// Without a table and without a provider charge there is nothing to report:
// usage is already on the wire as UsageEvent.
func TestRunCost_SilentWithNeitherSource(t *testing.T) {
	prov := &usageStampingProvider{usage: TokenUsage{PromptTokens: 100, TotalTokens: 100}}

	if _, seen := runForCost(t, prov); seen {
		t.Fatal("RunCostEvent must stay silent with no PriceTable and no provider cost")
	}
}

// mixedCostProvider reports a charge on its first call only, then loops a
// tool call so the Run makes several. Models a router fanning out to a
// self-pricing gateway and a vendor that reports nothing.
type mixedCostProvider struct {
	calls int
}

func (p *mixedCostProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.calls++
	if p.calls == 1 {
		return LLMResult{
			ToolCalls: []PendingToolCall{{ID: "c1", Name: "noop", ArgsJSON: `{}`}},
			Usage:     TokenUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, CostUSD: 0.05},
		}, nil
	}
	ch <- Event(ContentEvent{Text: "ok"})
	return LLMResult{
		Content: "ok",
		Usage:   TokenUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
	}, nil
}

// Dollars resolve per call, not once over the rollup: the priced call keeps
// its exact charge and the silent one is estimated from the table. Summing
// the table over total tokens would bill the gateway's call twice over.
func TestRunCost_MixedProvidersResolvePerCall(t *testing.T) {
	rc, seen := runForCost(t, &mixedCostProvider{},
		WithPriceTable(PriceTable{"m": {InputPerMTokens: 10, OutputPerMTokens: 20}}, "m"))
	if !seen {
		t.Fatal("RunCostEvent not emitted")
	}
	// 0.05 reported + (1000*$10 + 500*$20)/1M = 0.05 + 0.02.
	if !closeUSD(rc.USD, 0.07) {
		t.Fatalf("USD = %v, want 0.07 (0.05 reported + 0.02 estimated)", rc.USD)
	}
	if !closeUSD(rc.Usage.CostUSD, 0.05) {
		t.Fatalf("Usage.CostUSD = %v, want only the 0.05 that was actually reported", rc.Usage.CostUSD)
	}
	if rc.Usage.TotalTokens != 3000 {
		t.Fatalf("Usage.TotalTokens = %d, want 3000 across both calls", rc.Usage.TotalTokens)
	}
}

// costOnlyProvider bills without reporting token counts, which some gateways
// do. The usage gate keyed on TotalTokens alone would drop the charge.
type costOnlyProvider struct{}

func (costOnlyProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	ch <- Event(ContentEvent{Text: "ok"})
	return LLMResult{Content: "ok", Usage: TokenUsage{CostUSD: 0.004}}, nil
}

func TestRunCost_CostWithoutTokenCounts(t *testing.T) {
	rc, seen := runForCost(t, costOnlyProvider{})
	if !seen {
		t.Fatal("RunCostEvent not emitted for a provider that bills without token counts")
	}
	if !closeUSD(rc.USD, 0.004) {
		t.Fatalf("USD = %v, want 0.004", rc.USD)
	}
}

// A negative charge is nonsense; it must not surface as a credit, and the
// call falls back to the table estimate as if nothing were reported.
func TestRunCost_NegativeProviderCostIsNotACredit(t *testing.T) {
	prov := &usageStampingProvider{usage: TokenUsage{
		PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, CostUSD: -5,
	}}

	rc, seen := runForCost(t, prov,
		WithPriceTable(PriceTable{"m": {InputPerMTokens: 10, OutputPerMTokens: 20}}, "m"))
	if !seen {
		t.Fatal("RunCostEvent not emitted")
	}
	if !closeUSD(rc.USD, 0.02) {
		t.Fatalf("USD = %v, want the 0.02 table estimate, not a credit", rc.USD)
	}
	if !closeUSD(rc.Usage.CostUSD, 0) {
		t.Fatalf("Usage.CostUSD = %v, want 0 — a negative charge is dropped, not accumulated", rc.Usage.CostUSD)
	}
}

// BudgetTracker sums TokenUsage off the usage stream. Dropping CostUSD there
// would leave Usage() reporting zero spend for a session the provider billed.
func TestBudgetTracker_AccumulatesProviderCost(t *testing.T) {
	bt := NewBudgetTracker(0)
	h := bt.Handler()
	ctx := context.Background()

	h(ctx, "s", Event(UsageEvent{Usage: TokenUsage{TotalTokens: 100, CostUSD: 0.02}}))
	h(ctx, "s", Event(UsageEvent{Usage: TokenUsage{TotalTokens: 50, CostUSD: 0.01}}))
	// A negative charge must not refund the session through the stream.
	h(ctx, "s", Event(UsageEvent{Usage: TokenUsage{TotalTokens: 10, CostUSD: -1}}))

	got := bt.Usage("s")
	if got.TotalTokens != 160 {
		t.Fatalf("TotalTokens = %d, want 160", got.TotalTokens)
	}
	if !closeUSD(got.CostUSD, 0.03) {
		t.Fatalf("CostUSD = %v, want 0.03", got.CostUSD)
	}
}

// Rewind refunds every field or none: leaving cost behind would strand spend
// on a session whose tokens were already returned.
func TestBudgetTracker_RewindRefundsProviderCost(t *testing.T) {
	bt := NewBudgetTracker(0)
	h := bt.Handler()
	h(context.Background(), "s", Event(UsageEvent{Usage: TokenUsage{TotalTokens: 100, CostUSD: 0.05}}))

	bt.Rewind("s", TokenUsage{TotalTokens: 40, CostUSD: 0.02})
	if got := bt.Usage("s"); !closeUSD(got.CostUSD, 0.03) || got.TotalTokens != 60 {
		t.Fatalf("after rewind = %+v, want TotalTokens=60 CostUSD=0.03", got)
	}

	// Over-refunding floors at zero rather than going negative.
	bt.Rewind("s", TokenUsage{TotalTokens: 999, CostUSD: 999})
	if got := bt.Usage("s"); !closeUSD(got.CostUSD, 0) || got.TotalTokens != 0 {
		t.Fatalf("after over-refund = %+v, want both floored to zero", got)
	}
}
