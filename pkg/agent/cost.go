package agent

import (
	"context"
	"sync"
)

// ModelPricing carries per-million-token rates for one model. Adopters
// populate from their provider's pricing page; the library treats the
// numbers as opaque floats and multiplies through.
//
// Both fields are dollars per million tokens (e.g. Claude Sonnet at
// $3 in / $15 out is ModelPricing{InputPerMTokens: 3, OutputPerMTokens: 15}).
type ModelPricing struct {
	InputPerMTokens  float64
	OutputPerMTokens float64
}

// PriceTable maps a canonical model name to its pricing. Lookups are by
// exact key; adopters use whatever string they like (the provider's
// canonical name, an internal tier label) as long as it matches the
// AgentLoop.PriceModel setting.
type PriceTable map[string]ModelPricing

// Compute returns the dollar cost of usage under the named model's
// pricing, or zero when the model is unknown to the table. Negative
// inputs are clamped to zero so a buggy accumulator can't surface a
// "credit" event.
func (pt PriceTable) Compute(model string, usage TokenUsage) float64 {
	if pt == nil {
		return 0
	}
	p, ok := pt[model]
	if !ok {
		return 0
	}
	in := float64(usage.PromptTokens)
	out := float64(usage.CompletionTokens)
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	return (in*p.InputPerMTokens + out*p.OutputPerMTokens) / 1_000_000
}

// runCostKey is the ctx-value key for the per-Run usage accumulator.
type runCostKey struct{}

// runCostAcc accumulates TokenUsage across every LLM call inside one
// Run. The handleFinalAnswer path reads the totals and emits a
// RunCostEvent right before DoneEvent. Concurrency: callLLM is single-
// threaded per Run today, but the mutex keeps the contract safe in
// case a future speculation path emits Usage from a worker goroutine.
type runCostAcc struct {
	mu    sync.Mutex
	usage TokenUsage
}

func (a *runCostAcc) add(u TokenUsage) {
	a.mu.Lock()
	a.usage.PromptTokens += u.PromptTokens
	a.usage.CompletionTokens += u.CompletionTokens
	a.usage.TotalTokens += u.TotalTokens
	a.mu.Unlock()
}

func (a *runCostAcc) snapshot() TokenUsage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage
}

func withRunCostAcc(ctx context.Context, acc *runCostAcc) context.Context {
	return context.WithValue(ctx, runCostKey{}, acc)
}

func runCostAccFromContext(ctx context.Context) *runCostAcc {
	v, _ := ctx.Value(runCostKey{}).(*runCostAcc)
	return v
}

// installRunCostAccumulator stashes a fresh accumulator on ctx and
// returns it alongside a cleanup callback that emits RunCostEvent at
// the end of the Run. Caller pattern:
//
//	ctx, emitCost := al.installRunCostAccumulator(ctx, sessionKey, streamChan)
//	defer emitCost()
//
// When PriceTable is nil, the returned ctx is unchanged and emitCost
// is a no-op closure — zero allocation in the hot path beyond the
// branch on PriceTable. Every Run entry point that drives
// iterateMessages (runLogicLoop, continueLogicLoop) must call this so
// MaxIters / MaxToolCallsPerSession / fatal-error terminal paths all
// emit the cost rollup, not just the final-answer success path.
func (al *AgentLoop) installRunCostAccumulator(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent) (context.Context, func()) {
	if al.PriceTable == nil {
		return ctx, func() {}
	}
	ctx = withRunCostAcc(ctx, &runCostAcc{})
	return ctx, func() {
		al.emitRunCostIfConfigured(ctx, sessionKey, streamChan)
	}
}
