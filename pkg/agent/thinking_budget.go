package agent

import "context"

// ThinkingBudget is a provider-neutral hint for extended reasoning effort.
// Providers translate the integer to the closest native knob they support:
//
//   - Anthropic Claude 4.x (extended thinking):
//     attaches thinking: {type:"enabled", budget_tokens: N}. Values below
//     the API minimum (1024) are raised to that floor.
//   - OpenAI o-series (reasoning_effort): values > 0 map by threshold —
//     (0, 2048] → "low", (2048, 8192] → "medium", >8192 → "high".
//   - Providers without a reasoning knob (Gemini, stock gpt-4o, …) ignore it.
//
// Zero disables the feature — providers send no thinking/reasoning fields.
// Setting a budget never changes wire semantics for providers that ignore it,
// so callers can enable it globally without breaking non-reasoning backends.
type thinkingBudgetKey struct{}

// WithThinkingBudget returns ctx with an extended-thinking budget attached.
// The AgentLoop installs this before every GenerateStream call when its
// ThinkingBudget field is non-zero; provider adapters read it with
// ThinkingBudgetFromContext.
//
// Tokens ≤ 0 is treated as "no hint" and removes any budget from ctx.
func WithThinkingBudget(ctx context.Context, tokens int) context.Context {
	if tokens <= 0 {
		return context.WithValue(ctx, thinkingBudgetKey{}, 0)
	}
	return context.WithValue(ctx, thinkingBudgetKey{}, tokens)
}

// ThinkingBudgetFromContext returns the extended-thinking budget stored on
// ctx, or 0 when no budget was set. Provider adapters call this right before
// building the request and skip the thinking/reasoning field when zero.
func ThinkingBudgetFromContext(ctx context.Context) int {
	v, _ := ctx.Value(thinkingBudgetKey{}).(int)
	if v < 0 {
		return 0
	}
	return v
}
