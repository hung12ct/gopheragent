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
//
// pt and model are captured at install time and never mutated, so add
// can resolve dollars per call without reaching back into the loop.
type runCostAcc struct {
	mu    sync.Mutex
	usage TokenUsage
	usd   float64

	pt    PriceTable
	model string
}

func (a *runCostAcc) add(u TokenUsage) {
	// A negative charge is nonsense and is treated as "not reported",
	// matching PriceTable.Compute's clamp: neither should let a buggy
	// provider surface a credit.
	cost := u.CostUSD
	if cost < 0 {
		cost = 0
	}
	a.mu.Lock()
	a.usage.PromptTokens += u.PromptTokens
	a.usage.CompletionTokens += u.CompletionTokens
	a.usage.TotalTokens += u.TotalTokens
	a.usage.CostUSD += cost
	// Resolve dollars per call rather than once over the rollup. A Run
	// that mixes backends — a router fanning out to a gateway that
	// prices its own calls and a vendor that does not — would otherwise
	// charge table rates over tokens the provider already billed, or
	// drop the estimate for the calls that reported nothing.
	if cost > 0 {
		a.usd += cost
	} else {
		a.usd += a.pt.Compute(a.model, u)
	}
	a.mu.Unlock()
}

// snapshot returns the accumulated usage and the resolved dollar total.
func (a *runCostAcc) snapshot() (TokenUsage, float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage, a.usd
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
// The accumulator is installed unconditionally, including when
// PriceTable is nil. It used to be gated on the table, which silently
// dropped the exact figure reported by providers that bill per call —
// precisely the adopters who configure no table because they do not
// need to estimate. Whether a provider reports a cost is not knowable
// before the first call, and there is no config knob to gate on, so
// the cost is one small struct and one ctx value per Run against a Run
// that makes at least one network call. Deliberate, same reasoning as
// installDegradationAccumulator.
//
// Every Run entry point that drives iterateMessages (runLogicLoop,
// continueLogicLoop) must call this so MaxIters /
// MaxToolCallsPerSession / fatal-error terminal paths all emit the
// cost rollup, not just the final-answer success path.
func (al *AgentLoop) installRunCostAccumulator(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent) (context.Context, func()) {
	ctx = withRunCostAcc(ctx, &runCostAcc{pt: al.PriceTable, model: al.PriceModel})
	return ctx, func() {
		al.emitRunCostIfConfigured(ctx, sessionKey, streamChan)
	}
}
