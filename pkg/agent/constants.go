package agent

// Internal tuning constants for the agent loop. Pulled out of inline
// literals so they can be cited and adjusted in one place.
const (
	// runIterationStreamBuffer sizes the internal channel between the
	// loop goroutine and the SSE proxy goroutine in RunIterationStream.
	runIterationStreamBuffer = 50

	// llmProviderBuffer sizes the channel each LLM provider streams into
	// during a single GenerateStream call.
	llmProviderBuffer = 50

	// softLandingMargin is the number of iterations before MaxIters at
	// which the loop emits the soft-landing nudge thought event.
	softLandingMargin = 2

	// budgetWarnRatio is the fraction of MaxTokenBudget at which
	// aggressive context pruning kicks in (truncate tool arguments).
	budgetWarnRatio = 0.85
)
