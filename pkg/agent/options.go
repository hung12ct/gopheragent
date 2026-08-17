package agent

import (
	"time"

	"github.com/hung12ct/gopheragent/pkg/cache"
	"github.com/hung12ct/gopheragent/pkg/memory"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// Option configures an AgentLoop at construction time. Pass options to New
// (the recommended entry point) so loop configuration is captured up-front
// and the runtime state is built atomically:
//
//	loop := agent.New(sm, reg, llm,
//	    agent.WithMaxIters(15),
//	    agent.WithRetry(retryCfg),
//	    agent.WithHITL(confirmFn, 2*time.Minute),
//	    agent.WithPermissions(perms),
//	)
//
// Options compose left-to-right; later options override earlier ones for
// the same field. The zero option (nil) is allowed for conditional wiring:
//
//	opts := []agent.Option{agent.WithMaxIters(15)}
//	if cfg.cache != nil {
//	    opts = append(opts, agent.WithCache(cfg.cache))
//	}
//	loop := agent.New(sm, reg, llm, opts...)
type Option func(*AgentLoop)

// New constructs an AgentLoop with the given session manager, tool registry,
// LLM provider, and a set of functional options. New is the recommended
// entry point — it returns a fully-configured loop ready to call Run on.
//
// Defaults applied before options run:
//   - MaxIters = 15
//   - EmitThoughts = true (streaming methods); RunIteration suppresses
//     thoughts regardless
//   - AutoCacheSystem = true
//
// All options listed below override these defaults. Pass no options for a
// minimal loop that uses the defaults end-to-end.
func New(sessions SessionManager, registry *tools.Registry, llm LLMProvider, opts ...Option) *AgentLoop {
	al := &AgentLoop{
		Sessions:        sessions,
		Tools:           registry,
		LLM:             llm,
		MaxIters:        15,
		EmitThoughts:    true,
		AutoCacheSystem: true,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(al)
		}
	}
	return al
}

// Configure applies additional options to an already-constructed loop and
// returns it for chaining. Use it to set options the constructor did not take —
// notably wiring agent.WithTracer / agent.WithMeter onto a loop produced by the
// YAML builder, which does not accept Option values. Apply before the first Run;
// options run in order and later ones win, matching New. nil options are skipped.
//
//	loop, _, _, _ := builder.BuildFromYAMLWithSession(...)
//	loop.Configure(agent.WithTracer(tel.Tracer), agent.WithMeter(tel.Meter))
func (al *AgentLoop) Configure(opts ...Option) *AgentLoop {
	for _, opt := range opts {
		if opt != nil {
			opt(al)
		}
	}
	return al
}

// WithMaxIters caps the number of ReAct iterations per run. The loop emits
// MaxItersReachedEvent + LimitExhaustedEvent when the cap fires. Default: 15.
func WithMaxIters(n int) Option {
	return func(al *AgentLoop) { al.MaxIters = n }
}

// WithEmitThoughts controls whether streaming methods forward ThoughtEvent
// frames. Default: true. Set false to silence internal narration in
// production UIs; RunIteration suppresses thoughts regardless.
func WithEmitThoughts(emit bool) Option {
	return func(al *AgentLoop) { al.EmitThoughts = emit }
}

// WithBeforeHook registers a hook that fires before every iteration. Hooks
// can veto by returning a non-nil error (HookRejectedError surfaces it to
// the stream). Multiple registrations append; hooks fire in registration
// order. nil hooks are silently dropped.
func WithBeforeHook(h Hook) Option {
	return func(al *AgentLoop) {
		if h == nil {
			return
		}
		al.BeforeHooks = append(al.BeforeHooks, h)
	}
}

// WithBeforeLLMHook registers a per-call gate that fires right before every
// GenerateStream invocation (including retries). Return non-nil to abort
// the iteration — typical use is per-session spend caps via
// BudgetTracker.Guard.
func WithBeforeLLMHook(h BeforeLLMHook) Option {
	return func(al *AgentLoop) {
		if h == nil {
			return
		}
		al.BeforeLLMHooks = append(al.BeforeLLMHooks, h)
	}
}

// WithHITL wires the human-in-the-loop confirmation callback and an
// optional timeout. Tools whose Descriptor reports RequiresConfirmation
// fire the gate before execution. timeout=0 disables the deadline (gate
// blocks until the callback returns or ctx cancels). A nil fn leaves
// ConfirmHITL unset — HITL tools will auto-deny.
func WithHITL(fn ConfirmFunc, timeout time.Duration) Option {
	return func(al *AgentLoop) {
		al.ConfirmHITL = fn
		al.ConfirmHITLTimeout = timeout
	}
}

// WithConfirmPlan registers the plan-approval callback. Fires when the
// model calls exit_plan_mode while PlanMode is active. Returning true
// approves the plan and clears PlanMode; false sends the model a revise
// directive.
func WithConfirmPlan(fn ConfirmPlanFunc) Option {
	return func(al *AgentLoop) { al.ConfirmPlan = fn }
}

// WithCache enables tool result caching with trigram similarity matching.
// nil disables caching (default). Build a cache with cache.NewSearchCache.
func WithCache(c *cache.SearchCache) Option {
	return func(al *AgentLoop) { al.Cache = c }
}

// WithMaxTokenBudget sets the estimated token ceiling for the conversation
// context. The loop applies more aggressive pruning when the estimate
// crosses the budget. 0 (default) disables enforcement.
func WithMaxTokenBudget(n int) Option {
	return func(al *AgentLoop) { al.MaxTokenBudget = n }
}

// WithRetry configures exponential-backoff retry for transient LLM errors.
// nil disables retries (default). DefaultRetryConfig() gives a reasonable
// 3-retry start. Retries are skipped once content has started streaming.
func WithRetry(cfg *RetryConfig) Option {
	return func(al *AgentLoop) { al.Retry = cfg }
}

// WithOnEvent registers a per-event handler invoked for every emitted
// StreamEvent. Multiple registrations append; handlers fire in order. nil
// handlers are silently dropped.
func WithOnEvent(h EventHandler) Option {
	return func(al *AgentLoop) {
		if h == nil {
			return
		}
		al.EventHandlers = append(al.EventHandlers, h)
	}
}

// WithDynamicContext registers a per-call context provider. The returned
// string is appended to the system prompt for that call only (never
// persisted). See DynamicContextFunc for the hot-path contract.
func WithDynamicContext(fn DynamicContextFunc) Option {
	return func(al *AgentLoop) { al.DynamicContext = fn }
}

// WithOnToolResult registers a post-tool-execution hook that may rewrite
// the result string or veto it. See ToolResultHook for the contract.
func WithOnToolResult(h ToolResultHook) Option {
	return func(al *AgentLoop) { al.OnToolResult = h }
}

// WithToolSelector enables semantic tool-list filtering per turn. nil
// (default) means every registered tool is presented every turn. Build a
// selector with tools.NewSelector.
func WithToolSelector(s *tools.Selector) Option {
	return func(al *AgentLoop) { al.ToolSelector = s }
}

// WithoutToolChainingHint suppresses the automatic <output_of:...> hint
// the loop injects into the system prompt when multiple tools are
// registered. Use when you have documented chaining yourself or want to
// opt out of the scheduling syntax entirely.
func WithoutToolChainingHint() Option {
	return func(al *AgentLoop) { al.DisableToolChainingHint = true }
}

// WithSpeculativeTools enables parallel speculation: when a provider
// surfaces tool_call_ready mid-stream, the loop kicks off safe calls in
// parallel with the remaining LLM stream. Eligibility excludes
// HITL-gated tools, plan-mode tools, and <output_of:...>-dependent calls.
// Default: false. Only Anthropic emits the mid-stream signal today.
func WithSpeculativeTools(enable bool) Option {
	return func(al *AgentLoop) { al.SpeculativeTools = enable }
}

// WithToolErrorHintFormatter customizes the text written back to the model
// after a tool error. nil (default) uses defaultToolErrorHint. Override to
// inject domain-specific remediation advice the model can act on next turn.
func WithToolErrorHintFormatter(fn ToolErrorHintFormatter) Option {
	return func(al *AgentLoop) { al.ToolErrorHintFormatter = fn }
}

// WithReflect enables N serial self-critique passes after the model
// returns a final answer. Each pass appends a critique prompt and asks
// the model to revise. Set prompt to "" to use the neutral default. 0
// (default) disables reflection; cost and latency multiply by (1 + N).
func WithReflect(rounds int, prompt string) Option {
	return func(al *AgentLoop) {
		al.Reflect = rounds
		if prompt != "" {
			al.ReflectPrompt = prompt
		}
	}
}

// WithThinking turns on extended reasoning for providers that support
// it. The argument is a token hint; the Anthropic adapter clamps to
// (1024, MaxTokens). 0 (default) keeps requests on the non-reasoning
// path. Distinct from agent.WithThinkingBudget which is a ctx decorator
// used to override the per-iteration budget on a single call.
func WithThinking(tokens int) Option {
	return func(al *AgentLoop) { al.ThinkingBudget = tokens }
}

// WithPermissions wires a permission policy consulted for every tool
// call before HITL fires. See PermissionChecker / NewPermissionRuleSet.
func WithPermissions(p PermissionChecker) Option {
	return func(al *AgentLoop) { al.Permissions = p }
}

// WithMaxToolCallsPerTurn caps the wave size for a single iteration. 0
// (default) means unlimited. Calls past the cap are short-circuited with
// a synthesized tool-error and a thought event explaining the drop.
func WithMaxToolCallsPerTurn(n int) Option {
	return func(al *AgentLoop) { al.MaxToolCallsPerTurn = n }
}

// WithMaxParallelToolCalls caps how many tool calls run concurrently inside
// one dependency wave. Defaults to 8; pass 0 for unlimited. Nothing is
// dropped — calls over the cap wait for a slot, so results are unchanged and
// only peak resource use differs. Raise it for latency-bound tools (HTTP
// fan-out), lower it for tools that each hold a scarce resource (database
// connections, subprocesses).
func WithMaxParallelToolCalls(n int) Option {
	return func(al *AgentLoop) { al.MaxParallelToolCalls = n }
}

// WithMaxToolCallsPerSession caps cumulative tool calls across all
// iterations of a single Run. 0 (default) means unlimited. When the cap
// trips, the loop emits LimitExhaustedEvent and saves history.
func WithMaxToolCallsPerSession(n int) Option {
	return func(al *AgentLoop) { al.MaxToolCallsPerSession = n }
}

// WithoutAutoCacheSystem disables the auto-stamp of CacheHint=true on
// the first system message of every LLM call. Default behavior is to
// stamp (Anthropic prompt-cache prefix). Disable when you manage cache
// breakpoints by hand.
func WithoutAutoCacheSystem() Option {
	return func(al *AgentLoop) { al.AutoCacheSystem = false }
}

// WithMemory enables cross-session memory by attaching a Store and a
// loader configuration. On every Run the loop reads notes for the
// resolved scope (bounded by cfg) and appends them to the system
// message. Without this option the memory loader is a no-op at zero
// hot-path cost. Pair with WithMemoryScope to share memory across
// sessions for the same user/tenant, and with WithMemoryConsolidator
// to auto-distill closed sessions into notes.
//
// cfg's zero value applies the documented defaults: TokenBudget=500,
// MaxNotes=50. Override either field on the call site to tune the
// per-Run memory cost.
func WithMemory(store memory.Store, cfg MemoryConfig) Option {
	return func(al *AgentLoop) {
		al.Memory = store
		al.MemoryCfg = cfg
	}
}

// WithMemoryScope overrides the default scope resolver (which returns
// sessionKey unchanged). Use to share memory across sessions for the
// same user: typically read a user_id off ctx and return a stable scope
// key derived from it. nil keeps the default.
func WithMemoryScope(fn MemoryScopeFunc) Option {
	return func(al *AgentLoop) { al.MemoryScopeFn = fn }
}

// WithMemoryConsolidator wires a Consolidator that fires after every
// Run terminating in DoneEvent. Runs detached from the request ctx so
// HTTP disconnects do not abort the LLM call. Pass nil to disable
// (default). Setting a consolidator without WithMemory is permitted —
// the consolidator writes to its own Store reference and the loader
// reads from al.Memory; in practice the same store backs both.
func WithMemoryConsolidator(c *Consolidator) Option {
	return func(al *AgentLoop) { al.MemoryConsolidator = c }
}

// WithPriceTable supplies the rates used to estimate cost for LLM calls
// whose provider does not report one. The loop accumulates TokenUsage
// across every call in a Run and emits RunCostEvent right before
// DoneEvent, with USD from table[model].
//
// This is only needed for providers that bill silently. A provider that
// sets TokenUsage.CostUSD is already exact, and RunCostEvent fires for
// it whether or not a table is configured — leave this unset in that
// case rather than adding rates the loop will never consult. Adopters
// with router-style multi-model setups whose pricing varies per call
// and whose providers report nothing should still roll cost up
// themselves from UsageEvent.
//
// model is the key looked up in table for cost computation — pass
// the canonical name of the model this loop drives. Unknown keys
// produce a RunCostEvent with USD=0 but Usage still populated, so
// adopters with a dynamic pricing source can compute cost downstream.
func WithPriceTable(table PriceTable, model string) Option {
	return func(al *AgentLoop) {
		al.PriceTable = table
		al.PriceModel = model
	}
}
