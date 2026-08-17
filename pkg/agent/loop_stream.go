// Package agent provides the core ReAct loop, streaming, and orchestration for LLM agents.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/hung12ct/gopheragent/pkg/cache"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/memory"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// Hook defines a middleware function that runs before the agent loop.
type Hook func(ctx context.Context, sessionKey string, userInput string) error

// ConfirmFunc is called when a tool declares RequiresConfirmation().
// It blocks the loop until a human decision is made.
// Return true to approve execution, false to deny.
// The toolName and argsJSON are provided for display to the user.
type ConfirmFunc func(ctx context.Context, toolName string, argsJSON string) bool

// EventHandler is called for every StreamEvent emitted during the agent loop,
// including events from LLM providers. It runs synchronously in the loop goroutine.
// Use for metrics, structured logging, token tracking, or custom analytics.
// sessionKey identifies which conversation the event belongs to.
type EventHandler func(ctx context.Context, sessionKey string, ev StreamEvent)

// SessionManager is the minimal contract every history backend must provide.
// It covers the read/write/async-tasks/save/delete surface the agent loop
// itself touches. Optional capabilities (forking, querying, soft delete) are
// exposed via the SessionForker / SessionQueryable / SoftDeletable
// interfaces below — call sites that need them should type-assert at the
// boundary instead of demanding every backend implement features it does
// not need. This keeps the core interface within the project's "small
// interface" budget (CLAUDE.md: 3–6 methods).
type SessionManager interface {
	// History returns the persisted message log for sessionKey. Backends that
	// cannot read return the error; an unseen session returns (nil, nil).
	History(ctx context.Context, sessionKey string) ([]history.Message, error)
	// SaveHistory atomically replaces the persisted log with msgs. Combines
	// what used to be SetHistory + Save into one call so backends with real
	// transactions (MySQL) can commit in a single round trip.
	SaveHistory(ctx context.Context, sessionKey string, msgs []history.Message) error
	// AsyncTasks returns the snapshot of background tasks parked on sessionKey.
	// Backends that cannot read return the error; an unseen session returns
	// (empty map, nil).
	AsyncTasks(ctx context.Context, sessionKey string) (map[string]history.AsyncTask, error)
	// SaveAsyncTasks atomically replaces the task snapshot. Same atomicity
	// rationale as SaveHistory.
	SaveAsyncTasks(ctx context.Context, sessionKey string, tasks map[string]history.AsyncTask) error
	// Delete removes all in-memory and persisted state for sessionKey. Used to
	// clean up ephemeral sub-agent and async-worker sessions after they
	// complete so they do not accumulate in storage. Deleting a non-existent
	// session is a no-op (returns nil). For end-user "delete this conversation"
	// flows that need undelete, prefer the SoftDeletable capability.
	Delete(ctx context.Context, sessionKey string) error
}

// SessionForker is the optional capability for backends that support
// forking a session at a given message index. Used by ForkAtLastUser and
// any "branch this conversation" UI flow. Backends without forking simply
// don't implement this interface; callers type-assert and report a clear
// error when the capability is missing.
type SessionForker interface {
	// Fork creates a new session whose message history is a copy of the first
	// atIndex messages from sessionKey. atIndex is clamped to the source length
	// and snapped backward to a "safe" boundary — the resulting prefix never
	// ends mid tool-call / tool-result group, so the forked session is always
	// valid input to the agent loop. The behavior summary is copied; async
	// tasks are not (they belong to the original session runtime).
	//
	// Returns the generated new session key. Fails if sessionKey does not exist
	// or atIndex is negative.
	Fork(ctx context.Context, sessionKey string, atIndex int) (string, error)
}

// SessionQueryable is the optional capability for backends that support
// listing sessions by key prefix. Used by sidebars, admin UIs, and
// reporting tools. Backends that only need point lookups do not need to
// implement this.
type SessionQueryable interface {
	// Query returns metadata for sessions whose key starts with prefix.
	// Pass an empty prefix to list everything. Soft-deleted sessions are
	// filtered out unless opts.IncludeDeleted is true. Result order honors
	// opts.OrderBy (default: most recently updated first). Limit/Offset
	// drive pagination.
	Query(ctx context.Context, prefix string, opts history.SessionQueryOpts) ([]history.SessionMeta, error)
}

// SessionTitler is the optional capability for backends that can attach a
// human-readable title to a session — typically the auto-generated label
// from builtin.GenerateTitle rendered in a sidebar / picker UI. Backends
// that have no place to store metadata (ephemeral in-process caches)
// simply skip this capability; the SessionMeta.Title field stays empty.
//
// Adopters call SetTitle once per session, usually from the handler that
// receives EventTypeSessionCreated. The title is then surfaced on every
// subsequent Query result for that session.
type SessionTitler interface {
	// SetTitle records title against sessionKey. Calling with an empty
	// string clears any previously-recorded title. Setting a title on a
	// session the backend has not yet seen is permitted (the title is
	// retained and joined to the session record once it is created).
	SetTitle(ctx context.Context, sessionKey string, title string) error
}

// SoftDeletable is the optional capability for backends that support
// reversible deletion via a tombstone. Backends that only do hard deletes
// (e.g. ephemeral in-process caches) do not need to implement this.
type SoftDeletable interface {
	// SoftDelete marks sessionKey as deleted without removing data; reads
	// (GetHistory / Query default) treat it as missing, but the row stays
	// behind so Restore can undo the action. No-op if already deleted.
	SoftDelete(ctx context.Context, sessionKey string) error
	// Restore clears the deletion timestamp set by SoftDelete. No-op if
	// the session is not soft-deleted.
	Restore(ctx context.Context, sessionKey string) error
	// PurgeDeletedBefore hard-deletes every soft-deleted session whose
	// DeletedAt is strictly older than `before`. Returns the count purged.
	// Pair with a periodic goroutine to bound storage growth from the
	// soft-delete tail.
	PurgeDeletedBefore(ctx context.Context, before time.Time) (int, error)
}

// PendingToolCall represents a single tool invocation requested by the LLM.
type PendingToolCall struct {
	ID       string
	Name     string
	ArgsJSON string
}

// TokenUsage carries per-call token accounting returned by an LLM provider.
// Providers that do not report usage leave the fields zero.
//
// CostUSD is the dollar amount the provider itself billed for the call, for
// the backends that return one (gateways that route across vendors typically
// do). Leave it zero when the provider reports no cost: the loop then falls
// back to estimating from AgentLoop.PriceTable, and a zero here means "not
// reported", never "free". A reported cost is the actual charge and beats any
// table estimate — it already accounts for the model the gateway picked,
// cache discounts, and per-vendor rates a static table cannot track.
type TokenUsage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
}

// LLMResult represents the structured response from the LLM provider.
// Content is optional — the agent loop accumulates content from "content" stream events.
// Providers may still set Content for backward compatibility, but it is only used as a
// fallback when no content events were emitted on the stream.
// ToolCalls must be populated when the model wants to invoke tools.
// Usage carries token accounting when the provider reports it.
type LLMResult struct {
	Content   string
	ToolCalls []PendingToolCall
	Usage     TokenUsage
}

// BeforeLLMHook fires immediately before each GenerateStream call. Returning
// an error aborts the iteration (the loop emits an error event and exits).
// Use for per-session budget enforcement, rate limiting, or policy checks.
// estimatedTokens is a rough (chars/4) pre-call estimate of the prompt size.
type BeforeLLMHook func(ctx context.Context, sessionKey string, estimatedTokens int) error

// ToolResultHook fires after every tool execution — success or failure —
// before the result reaches the LLM context, the inline-render path, the
// cache, or the anti-loop tracker. Adopters get one place to observe every
// call instead of pairing this hook with an EventTypeError handler.
//
// On the success path, the returned string replaces the original result and a
// non-nil error from the hook converts the call into a tool error (formatted
// via formatToolError). On the error path, execErr is the tool's failure;
// returning a nil error from the hook recovers the call (the returned string
// becomes the result the LLM sees), while returning a non-nil error replaces
// execErr — useful for downgrading a noisy provider error to a neutral
// retry-hint string.
//
// Typical uses: audit logging (log every call regardless of success), rewrite
// URLs (e.g. local -> CDN), redact secrets, normalize formats, post-validate
// successful results, or veto/recover failures.
//
// toolCallID is the agent-generated correlation ID matching the
// EventTypeToolCall event for this dispatch — log it alongside your records
// so entry events and post-execution audit lines pair up. ctx is the per-tool
// ctx (carries WithProgressFunc / WithSubAgentEmitter / WithDynamicContextFunc
// / WithToolCallID).
//
// structured is non-nil only when the executed tool implements
// tools.StructuredResult. Hooks that mutate output (URL rewrites, redaction,
// post-validation) should prefer reading typed fields off structured rather
// than regex-parsing result.
type ToolResultHook func(ctx context.Context, toolCallID, toolName, argsJSON, result string, structured any, execErr error) (string, error)

// LLMProvider abstracts the model backend (OpenAI/Gemini/Claude).
type LLMProvider interface {
	GenerateStream(ctx context.Context, memory []history.Message, availableTools *tools.Registry, streamChan chan<- StreamEvent) (LLMResult, error)
}

// LLMCapabilities describes what a provider adapter can put on the wire.
// It does not promise that every model behind a compatible endpoint supports
// the feature; dynamic model capability checks remain the caller's job.
type LLMCapabilities struct {
	ImageInput       bool
	StructuredOutput bool
}

// CapabilityProvider optionally reports an adapter's transport features, so a
// consumer that requires one (a judge that compares images, a caller that
// depends on schema enforcement) can reject an unsuitable provider at
// construction instead of discovering it from a confident, wrong answer.
//
// Three rules make the signal trustworthy; breaking any of them turns it back
// into a guess:
//
//   - Absence means unknown, not false. A provider that does not implement
//     this interface makes no claim, and callers decide how to treat that.
//     Implement it on every adapter — including fakes that support nothing,
//     which is exactly the case the interface exists to catch.
//   - A decorator around a single LLMProvider must forward that provider's
//     report, and must not implement this interface when the wrapped provider
//     does not — pick the concrete type at construction. Otherwise wiring
//     tracing silently erases a capability the caller checks for, or invents a
//     claim the underlying adapter never made. A multiplexer that cannot know
//     its target in advance (a router) cannot do that, and instead reports the
//     intersection of everything it might dispatch to, counting an undeclared
//     member as supporting nothing: that errs toward a loud rejection at
//     construction rather than a silent wrong answer at run time.
//   - The report describes the adapter, not the model. A gateway that fronts
//     both text-only and multimodal models answers for the transport it
//     speaks; selecting a model that honours it stays the caller's job.
type CapabilityProvider interface{ Capabilities() LLMCapabilities }

// AgentLoop orchestrates the ReAct loop.
type AgentLoop struct {
	Sessions     SessionManager
	Tools        *tools.Registry
	LLM          LLMProvider
	MaxIters     int
	EmitThoughts bool
	BeforeHooks  []Hook

	// ConfirmHITL is called when a tool requires human approval.
	// If nil, HITL tools are auto-denied with a rejection message.
	// If set, the loop blocks until the function returns.
	ConfirmHITL ConfirmFunc

	// ConfirmHITLTimeout caps how long the HITL gate waits for ConfirmHITL
	// to return. Zero (default) means no timeout — the gate blocks until the
	// callback returns or ctx is cancelled. When set, the gate wraps the
	// callback ctx with this deadline; a ConfirmHITL that respects ctx will
	// return false on expiry, the gate emits EventTypeHITLTimedOut, and the
	// model receives a timeout-specific directive (distinct from a user
	// denial) so it can ask the user to retry rather than seek an alternative.
	ConfirmHITLTimeout time.Duration

	// Cache provides optional tool result caching with trigram similarity matching.
	// If nil, caching is disabled. Set via cache.NewSearchCache().
	Cache *cache.SearchCache

	// MaxTokenBudget is the estimated token ceiling for the conversation context.
	// 0 disables budget enforcement. When the estimated token count exceeds this
	// value, the loop applies more aggressive context pruning before the next LLM
	// call. Estimation uses a 4-chars/token heuristic (~±20% accuracy for English).
	MaxTokenBudget int

	// EventHandlers is called for every StreamEvent emitted during the loop.
	// Multiple handlers can be registered; all fire in order for each event.
	// Register handlers via OnEvent for a fluent API.
	EventHandlers []EventHandler

	// Retry controls exponential-backoff retry for transient LLM errors.
	// nil disables retries. Use DefaultRetryConfig() for sensible defaults.
	// Retries are skipped when content has already started streaming to the client.
	Retry *RetryConfig

	// BeforeLLMHooks fire right before every GenerateStream call (including
	// retries). Any non-nil error aborts the iteration. Useful for per-session
	// spend caps and policy enforcement; pair with a BudgetTracker for a
	// ready-made implementation.
	BeforeLLMHooks []BeforeLLMHook

	// DynamicContext, when non-nil, is invoked before every GenerateStream
	// call and its return value is appended to the system prompt for that
	// call only (never persisted to session history). See DynamicContextFunc
	// for the hot-path contract and prompt-cache interaction notes.
	DynamicContext DynamicContextFunc

	// OnToolResult, when non-nil, runs after every successful tool execution
	// and may rewrite the result string (or veto it via a non-nil error)
	// before it reaches the LLM context, the inline-render path, the cache,
	// and the anti-loop tracker. See ToolResultHook for the contract.
	OnToolResult ToolResultHook

	// ToolSelector, when non-nil, filters the tool list presented to the LLM
	// each turn by semantic similarity between the latest user message and
	// tool descriptions. Reduces prompt-token cost and improves tool-choice
	// accuracy for large catalogs (50+ tools). See tools.NewSelector. A nil
	// selector means every registered tool is presented every turn (default).
	ToolSelector *tools.Selector

	// DisableToolChainingHint suppresses the automatic system-prompt snippet
	// that teaches the LLM to chain tool calls via <output_of:ID.path>. By
	// default (false), the hint is appended to the system message on every
	// LLM call when at least two tools are registered and the prompt does
	// not already contain the syntax. Set to true when you have documented
	// the syntax yourself or explicitly want to opt out of scheduling.
	DisableToolChainingHint bool

	// sessionPlanMode tracks per-session plan-mode state, keyed by
	// sessionKey, value bool (true = plan mode active for that session).
	// Sessions with no entry are in normal mode.
	//
	// Per-session storage matters: a single AgentLoop instance is the
	// canonical pattern for serving N concurrent HTTP sessions, and plan
	// mode is a property of the user's conversation, not of the loop.
	// Approving Alice's plan must not exit plan mode for Bob.
	//
	// sync.Map fits the access pattern: reads are hot (every LLM iteration
	// and every speculation eligibility check), writes are rare (entering
	// or leaving plan mode), and keys are disjoint across sessions. The
	// public surface is IsPlanMode / SetPlanMode / ClearSession.
	sessionPlanMode sync.Map

	// ConfirmPlan runs when the model calls exit_plan_mode while PlanMode is
	// true. Returning true approves the plan and clears PlanMode; false
	// denies and the tool result asks the model to revise. When nil, the
	// loop emits an action_required event and auto-denies until the caller
	// approves out-of-band and re-runs the iteration with PlanMode=false.
	ConfirmPlan ConfirmPlanFunc

	// SpeculativeTools, when true, executes safe tool calls in parallel with
	// the remaining LLM stream as soon as their arguments finish streaming —
	// trimming one tail-latency round trip per call, ~200ms on average for
	// Claude. Eligibility is conservative: HITL-gated tools, exit_plan_mode,
	// calls that reference <output_of:...>, and anything while PlanMode is
	// active are never speculated. See shouldSpeculate for the exact rules.
	//
	// Enabling this has no effect unless the provider emits mid-stream
	// tool_call_ready events; today that is the Anthropic adapter. Other
	// providers fall back to the original post-stream execution path.
	SpeculativeTools bool
	// ToolErrorHintFormatter customizes the text the loop writes back to
	// the model when a tool execution returns an error. nil uses
	// defaultToolErrorHint, which wraps the raw error with a short
	// structured retry hint. Override to inject domain remediation advice
	// (SQL error → "check column exists", filesystem error → "verify the
	// path is under /tmp", etc.); the model re-reads the tool result on
	// every subsequent turn, so better wording compounds.
	ToolErrorHintFormatter ToolErrorHintFormatter

	// Reflect runs N serial self-critique passes after the model produces a
	// final answer with no pending tool calls. Each pass appends a synthetic
	// critique prompt and asks the model to revise its answer; the final
	// round's text becomes the canonical response saved to history and
	// emitted to callers — unless Scorer is set, in which case the
	// best-scoring round wins instead. 0 (default) disables reflection
	// entirely.
	//
	// Reflection is opt-in because it multiplies latency and token cost by
	// (1 + N). It targets correctness-critical tasks — SQL generation, code
	// synthesis, multi-step analysis — where a second-pass review routinely
	// catches mistakes the first pass missed.
	Reflect int

	// ReflectPrompt overrides the default critique instruction appended at
	// each reflection round. Leave empty to use defaultReflectPrompt, which
	// is a neutral "review and revise if wrong; otherwise repeat verbatim"
	// prompt suitable for most tasks. Override with domain-specific criteria
	// (e.g. "check every JOIN condition binds a valid FK") when the default
	// misses task-specific pitfalls.
	ReflectPrompt string

	// Scorer ranks candidate answers so a self-critique pass keeps the
	// best round rather than the last one. nil (default) preserves the
	// historical last-wins behavior: every non-empty, textually different
	// revision is accepted, so a critique round can make the answer worse
	// and the earlier text is unrecoverable.
	//
	// With a Scorer set, the model's original answer is scored as round 0
	// and each revision must beat the best score so far to be adopted; a
	// revision that scores lower is discarded and the next round critiques
	// the best answer instead. The kept round's score rides along on
	// ReflectedEvent so a UI can show why.
	//
	// Costs one Score call per round plus one for the original. See Scorer
	// for the latency and token-spend caveats.
	//
	// Only the self-critique path consumes this today, so setting it with
	// Reflect == 0 is inert. The interface is deliberately generic — a
	// best-of-K runner is the intended second consumer.
	Scorer Scorer

	// ThinkingBudget turns on extended reasoning for providers that support
	// it. It is a token hint, not a hard cap — see WithThinkingBudget for the
	// per-provider mapping. Set to 0 (default) to keep requests on the normal
	// non-reasoning path. Anthropic requires a minimum of 1024 tokens and the
	// budget must be strictly less than the provider's MaxTokens; the
	// Anthropic adapter clamps accordingly.
	ThinkingBudget int

	// Permissions, when non-nil, is consulted for every tool call before
	// the HITL prompt fires. Allow bypasses RequiresConfirmation(); Deny
	// short-circuits even for tools that would otherwise run silently;
	// Prompt defers to the existing ConfirmHITL flow. Use a
	// PermissionRuleSet for the built-in glob-based DSL, or implement
	// PermissionChecker directly for custom logic (OPA, database-backed
	// ACLs, etc).
	Permissions PermissionChecker

	// MaxToolCallsPerTurn caps how many tool calls the loop will execute for
	// a single LLM turn. 0 (default) means unlimited. When the model returns
	// more than N calls, the first N run normally and the remainder are
	// dropped with a synthesized tool-error message so the model can see why
	// they did not execute. A "thought" event announces the truncation.
	MaxToolCallsPerTurn int

	// MaxParallelToolCalls caps how many tool calls execute concurrently
	// within one dependency wave. Defaults to defaultMaxParallelToolCalls;
	// 0 means unlimited. Unlike MaxToolCallsPerTurn this drops nothing — a
	// call over the cap waits for a slot, so the wave's result set is
	// identical either way. It bounds live resource use (sockets,
	// subprocesses, provider rate limits) when a model emits a wide fan-out.
	MaxParallelToolCalls int

	// MaxToolCallsPerSession caps the cumulative number of tool calls
	// scheduled across all iterations of a single Run. 0 (default) means
	// unlimited. Distinct from MaxToolCallsPerTurn, which only bounds the
	// wave size within one iteration — anti-loop catches identical-arg
	// loops, but identical-tool-different-args could otherwise run
	// unchecked across many ReAct iterations. When the cap is crossed,
	// the loop emits an error event carrying ErrMaxToolCallsPerSession
	// and saves the session before returning.
	MaxToolCallsPerSession int

	// RequestInvariant, when set, receives a description of any broken
	// request-path invariant: the pipeline writing through to stored
	// history, or a request that a re-derivation cannot reproduce. Nil
	// (default) disables the check entirely — no snapshot is taken and the
	// cost is one nil comparison per iteration. Set it with
	// WithRequestInvariant in development and staging.
	RequestInvariant RequestViolationFunc

	// AutoCacheSystem, when true, stamps Message.CacheHint=true on the first
	// system message of every LLM call. On Anthropic this promotes the
	// entire system prompt into the prompt-cache prefix, typically cutting
	// input-token cost on that block to ~10% of normal for repeat turns
	// within a 5-minute window. Ignored by providers that don't honor
	// CacheHint (OpenAI, Gemini). Idempotent.
	//
	// NewAgentLoop defaults this to true — struct-literal construction
	// (&AgentLoop{...}) gets zero-value false and must opt in explicitly.
	AutoCacheSystem bool

	// Memory, when non-nil, enables cross-session note injection. On
	// every Run the loop reads notes for the resolved scope (bounded
	// by MemoryCfg), formats them with memory.FormatNotes, and
	// appends the block to the system message via a sentinel-tagged
	// path that's idempotent across LLM retries within an iteration
	// and stable across iterations within a Run. nil disables the
	// loader at zero hot-path cost.
	Memory memory.Store

	// MemoryCfg bounds the loader's per-Run cost. Zero-valued fields
	// resolve to documented defaults (TokenBudget=500, MaxNotes=50).
	// See MemoryConfig for details.
	MemoryCfg MemoryConfig

	// MemoryScopeFn resolves the scope key used for memory reads and
	// for any Consolidator auto-fires. nil (default) returns sessionKey
	// unchanged, isolating memory per-conversation; override to share
	// memory across sessions for the same user/tenant (typical pattern
	// is to read a user_id from ctx and return "user:" + id).
	MemoryScopeFn MemoryScopeFunc

	// MemoryConsolidator, when non-nil, fires after every Run that
	// terminates in DoneEvent. Runs in a detached goroutine that
	// inherits ctx values but is immune to ctx cancellation — the
	// caller's request lifetime never aborts an in-flight consolidation.
	// Set this together with Memory; setting it without a store is a
	// configuration error caught at Consolidate time.
	MemoryConsolidator *Consolidator

	// PriceTable supplies the rates used to estimate the dollar cost of
	// LLM calls whose provider reports none. The loop accumulates
	// TokenUsage across every call in a Run and emits a RunCostEvent
	// right before DoneEvent. Leaving it nil does not disable the
	// rollup: a provider that sets TokenUsage.CostUSD is already exact
	// and reports through the same event. Adopters running multi-model
	// router setups whose pricing varies per call, against providers
	// that report nothing, should leave this nil and roll cost up
	// themselves from UsageEvent.
	PriceTable PriceTable

	// PriceModel is the key looked up in PriceTable for cost
	// computation. Set to the canonical name of the model this loop
	// drives (e.g. "claude-sonnet-4-6"). Unknown keys produce a
	// RunCostEvent with USD=0 and Usage still populated.
	PriceModel string

	// confirmHITLWarnOnce gates the one-time misconfig warning emitted by
	// runHITLGate when ConfirmHITL is nil. Loop-scoped so each instance
	// warns independently; never reset across calls.
	confirmHITLWarnOnce sync.Once

	// bgWg tracks background goroutines the loop launches and detaches
	// from the caller's ctx (today: consolidator after every Run).
	// Shutdown waits on this counter so a graceful HTTP teardown can
	// wait for in-flight memory writes to complete instead of dropping
	// them. Per-Run runLogicLoop is *not* tracked here — it terminates
	// via the caller's ctx and the iterator's range-loop exit.
	bgWg sync.WaitGroup

	// tracer, when non-nil, opens a span per ReAct iteration (WithTracer).
	// iterHist, when non-nil, records per-iteration latency in seconds; it is
	// built once by WithMeter. Both nil (default) means zero instrumentation
	// cost on the hot path (see telemetry.go).
	tracer   trace.Tracer
	iterHist metric.Float64Histogram
}

// NewAgentLoop creates a new agent with the given session manager, tool registry, and LLM provider.
// Optional hooks run before each iteration for security/policy enforcement.
func NewAgentLoop(sessions SessionManager, registry *tools.Registry, llm LLMProvider, hooks ...Hook) *AgentLoop {
	return &AgentLoop{
		Sessions:             sessions,
		Tools:                registry,
		LLM:                  llm,
		MaxIters:             15,
		EmitThoughts:         true,
		BeforeHooks:          hooks,
		AutoCacheSystem:      true,
		MaxParallelToolCalls: defaultMaxParallelToolCalls,
	}
}

// OnEvent registers an EventHandler and returns the AgentLoop for fluent chaining.
//
//	loop.OnEvent(metrics.Track).OnEvent(logger.Log)
func (al *AgentLoop) OnEvent(h EventHandler) *AgentLoop {
	al.EventHandlers = append(al.EventHandlers, h)
	return al
}

// IsPlanMode reports whether plan mode is active for sessionKey. Sessions
// with no entry are treated as normal mode (false). Safe to call
// concurrently with SetPlanMode and with an in-flight RunIterationStream.
//
// Lifecycle: an entry exists from SetPlanMode(key, true) until either
// SetPlanMode(key, false) or ClearSession(key). Callers that delete a
// session (e.g. on HTTP disconnect or sub-agent finish) should call
// ClearSession to avoid orphaning entries for abandoned plan-mode sessions.
func (al *AgentLoop) IsPlanMode(sessionKey string) bool {
	v, ok := al.sessionPlanMode.Load(sessionKey)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// SetPlanMode toggles plan mode for sessionKey. Setting false removes the
// session's entry, keeping the map small for the common no-plan-mode path
// (most sessions never enter plan mode). Safe to call concurrently with
// IsPlanMode and with an in-flight RunIterationStream — useful for UI
// controls that flip plan mode mid-conversation.
func (al *AgentLoop) SetPlanMode(sessionKey string, v bool) {
	if v {
		al.sessionPlanMode.Store(sessionKey, true)
	} else {
		al.sessionPlanMode.Delete(sessionKey)
	}
}

// ClearSession removes any per-session state the AgentLoop holds for
// sessionKey. Today this is just plan-mode state; future per-session fields
// added to AgentLoop should be cleaned up here too. Idempotent — safe to
// call on sessions that never entered plan mode.
//
// Call this when you delete a session (e.g. SessionManager.DeleteSession,
// HTTP disconnect, sub-agent finish) so abandoned plan-mode sessions do
// not orphan map entries.
func (al *AgentLoop) ClearSession(sessionKey string) {
	al.sessionPlanMode.Delete(sessionKey)
}

// emit sends ev to streamChan and fires all registered EventHandlers.
// It must only be called from within the runLogicLoop goroutine.
//
// The streamChan send is bare (no ctx select) on purpose: terminal
// events (errors, done) must reach the consumer for it to know the loop
// finished. The call-path patterns guarantee a reader — RunIterationStream
// installs a proxy goroutine that drains internalChan on ctx.Done, and
// RunIteration drains synchronously — so the bare send never blocks
// indefinitely.
func (al *AgentLoop) emit(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, ev StreamEvent) {
	streamChan <- ev
	for _, h := range al.EventHandlers {
		safeCallHandler(h, ctx, sessionKey, ev)
	}
}

// safeCallHandler invokes h with panic recovery so a buggy event handler
// can never take down the agent loop. Recovered panics are logged to
// stderr and the next handler runs unaffected.
func safeCallHandler(h EventHandler, ctx context.Context, sessionKey string, ev StreamEvent) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[gopheragent] event handler panicked: %v (session=%q event=%q)", r, sessionKey, ev.Type)
		}
	}()
	h(ctx, sessionKey, ev)
}

// StreamEvent represents a chunk of data sent via SSE.
// When Type is "error", Err holds the structured error so callers can use errors.Is/As.
//
// Source and ParentID are populated when the event originated inside a
// sub-agent (or async worker) that forwarded it to a parent stream, giving
// consumers enough context to group, filter, or render a multi-agent timeline.
//   - Source  — tag describing the emitter, e.g. "subagent:researcher". When a
//     chain forwards through multiple sub-agents, tags are prepended with ">"
//     so the outermost hop appears first ("subagent:A>subagent:B").
//   - ParentID — the parent session key at the top of the forwarding chain,
//     useful for correlating every event back to the user-facing session.
//
// An event whose Source is empty originates from the receiving agent itself.
type StreamEvent struct {
	// Type tags the kind of event. Always matches Payload's eventType().
	Type StreamEventType `json:"type"`
	// Source is the emitter tag for forwarded events (e.g. "subagent:foo").
	// Empty for events originating from the receiving agent itself.
	Source string `json:"source,omitempty"`
	// ParentID is the parent session key at the top of the forwarding chain,
	// useful for correlating every event back to the user-facing session.
	ParentID string `json:"parent_id,omitempty"`
	// Payload carries the typed event data. Never nil for events produced by
	// the framework; consumers reaching across the wire should still tolerate
	// nil from a malformed envelope.
	Payload EventPayload `json:"-"`
}

// MarshalJSON serializes the event using a tagged-union envelope. Wire shape:
//
//	{"type":"content","source":"...","parent_id":"...","payload":{"text":"hi"}}
//
// SSE consumers can read `type` to dispatch and `payload` to decode the
// inner structure with one round trip per event.
func (ev StreamEvent) MarshalJSON() ([]byte, error) {
	envelope := struct {
		Type     StreamEventType `json:"type"`
		Source   string          `json:"source,omitempty"`
		ParentID string          `json:"parent_id,omitempty"`
		Payload  EventPayload    `json:"payload,omitempty"`
	}{
		Type:     ev.Type,
		Source:   ev.Source,
		ParentID: ev.ParentID,
		Payload:  ev.Payload,
	}
	return json.Marshal(envelope)
}

// UnmarshalJSON decodes the envelope and routes the payload bytes through
// decodePayload. Unknown event types round-trip via UnknownEvent so
// forward-compatible consumers do not crash.
func (ev *StreamEvent) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Type     StreamEventType `json:"type"`
		Source   string          `json:"source"`
		ParentID string          `json:"parent_id"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	ev.Type = envelope.Type
	ev.Source = envelope.Source
	ev.ParentID = envelope.ParentID
	ev.Payload = decodePayload(envelope.Type, envelope.Payload)
	return nil
}

// errEvent is a convenience constructor for ErrorEvent stream events with a
// typed error.
func errEvent(err error) StreamEvent {
	return Event(ErrorEvent{Err: err, Message: err.Error()})
}

// isFreshSessionHistory reports whether the slice returned by
// SessionManager.GetHistory looks like a never-before-seen session: empty,
// or a single system message (every backend seeds new sessions that way).
// Used to fire EventTypeSessionCreated as the first frame of a stream.
func isFreshSessionHistory(msgs []history.Message) bool {
	if len(msgs) == 0 {
		return true
	}
	return len(msgs) == 1 && msgs[0].Role == "system"
}

// Run streams events from the agent loop as a pull-based iterator. The caller
// ranges over it; the library owns the underlying goroutine and tears it down
// when the range loop exits — for any reason, including early break or ctx
// cancellation.
//
//	for ev := range agent.Run(ctx, sessionKey, msg) {
//	    if p, ok := ev.Payload.(agent.ErrorEvent); ok { return p.Err }
//	    // ... handle ev
//	    if userStopped { break }   // library cancels the loop, drains
//	}
//
// Thought events are filtered when AgentLoop.EmitThoughts is false (default).
// Errors are surfaced as ErrorEvent payloads in-stream; callers that want a
// terminal Go error type-assert on the payload. For the blocking "return the
// final answer" shape, use RunIteration.
func (al *AgentLoop) Run(ctx context.Context, sessionKey string, msg history.Message) iter.Seq[StreamEvent] {
	if msg.Role == "" {
		msg.Role = "user"
	}
	emitThoughts := al.EmitThoughts
	return func(yield func(StreamEvent) bool) {
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		internalChan := make(chan StreamEvent, runIterationStreamBuffer)
		go al.runLogicLoop(runCtx, sessionKey, msg, internalChan)
		yieldEvents(internalChan, yield, emitThoughts, cancel)
	}
}

// RunText is a string-input convenience wrapper around Run for text-only
// turns. The wrapped history.Message has Role="user" and Content=text.
func (al *AgentLoop) RunText(ctx context.Context, sessionKey, text string) iter.Seq[StreamEvent] {
	return al.Run(ctx, sessionKey, history.Message{Role: "user", Content: text})
}

// RunIteration provides a fast, blocking response interface (non-streaming).
// Thoughts are always suppressed — only final content and errors are returned.
// Implemented on top of Run: drains the iterator, collecting Content events
// into the returned string and surfacing the first ErrorEvent as a Go error.
func (al *AgentLoop) RunIteration(ctx context.Context, sessionKey string, userInput string) (string, error) {
	return al.RunIterationMessage(ctx, sessionKey, history.Message{Role: "user", Content: userInput})
}

// RunIterationMessage is the multimodal-aware variant of RunIteration. The
// caller controls the appended user message — set Parts to pass image bytes,
// ToolCallID for tool-result inputs, CacheHint for prompt-cache breakpoints,
// etc. Role defaults to "user" when empty.
func (al *AgentLoop) RunIterationMessage(ctx context.Context, sessionKey string, msg history.Message) (string, error) {
	if msg.Role == "" {
		msg.Role = "user"
	}
	// Suppress thoughts unconditionally for the blocking shape — callers that
	// want them should use Run with EmitThoughts=true.
	prevEmitThoughts := al.EmitThoughts
	al.EmitThoughts = false
	defer func() { al.EmitThoughts = prevEmitThoughts }()

	var buf strings.Builder
	var lastErr error
	for ev := range al.Run(ctx, sessionKey, msg) {
		// Events forwarded from sub-agents (Source != "") are observational only;
		// they must not be treated as the parent's own final answer or error.
		if ev.Source != "" {
			continue
		}
		switch p := ev.Payload.(type) {
		case ContentEvent:
			buf.WriteString(p.Text)
		case ReflectedEvent:
			// A reflection round produced a canonical revised answer — it
			// replaces whatever Source="" content was streamed earlier this
			// iteration so RunIteration always returns the final post-critique
			// text.
			if p.Text != "" {
				buf.Reset()
				buf.WriteString(p.Text)
			}
		case ErrorEvent:
			if p.Err != nil {
				lastErr = p.Err
			} else if p.Message != "" {
				lastErr = fmt.Errorf("agent: %s", p.Message)
			}
		}
	}
	return buf.String(), lastErr
}

// yieldEvents drains internalChan through yield, filtering thoughts when not
// requested and best-effort forwarding terminal frames when ctx cancels or
// the caller breaks early. The cancel func is the iterator's cleanup hook —
// invoking it stops the underlying runLogicLoop so we can finish draining.
func yieldEvents(internalChan chan StreamEvent, yield func(StreamEvent) bool, emitThoughts bool, cancel context.CancelFunc) {
	for ev := range internalChan {
		if ev.Type == EventTypeThought && !emitThoughts {
			continue
		}
		if !yield(ev) {
			// Caller broke early. Cancel and drain so the producer goroutine
			// finishes without blocking on the unbuffered/full channel.
			cancel()
			for range internalChan {
			}
			return
		}
	}
}

// latestUserMessage returns the Content of the most recent user message, or
// "" if no user message exists. Used by ToolSelector to embed per-turn intent.
func latestUserMessage(msgs []history.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// estimateTokens returns a rough token count using the 4-chars/token heuristic.
// Accurate within ~20% for English text; sufficient for MaxTokenBudget enforcement.
func estimateTokens(msgs []history.Message) int {
	var total int
	for _, m := range msgs {
		total += len(m.Content) / 4
		for _, tc := range m.ToolCalls {
			total += len(tc.Arguments) / 4
		}
	}
	return total
}

// saveSession persists history using the caller's ctx with cancellation
// stripped — values (trace IDs, user IDs) propagate to value-aware session
// backends but a cancelled request ctx will not abort persistence.
func (al *AgentLoop) saveSession(ctx context.Context, sessionKey string, msgs []history.Message) {
	saveCtx := context.WithoutCancel(ctx)
	if err := al.Sessions.SaveHistory(saveCtx, sessionKey, msgs); err != nil {
		log.Printf("[gopheragent] session save error for %q: %v", sessionKey, err)
	}
}

// runLogicLoop holds the actual execution context separated from the stream proxy.
// userMsg is appended verbatim — callers control role / content / parts /
// cache hints. The string-based RunIteration{,Stream} entrypoints wrap a
// plain text message; RunIteration{,Stream}Message exposes the full shape
// for multimodal input (image bytes, mixed parts).
func (al *AgentLoop) runLogicLoop(ctx context.Context, sessionKey string, userMsg history.Message, streamChan chan<- StreamEvent) {
	defer close(streamChan)
	ctx = WithSessionKey(ctx, sessionKey)

	for _, hook := range al.BeforeHooks {
		if hook == nil {
			continue
		}
		// Hooks predate multimodal input; pass the text Content so the
		// existing string-based contract is preserved. Parts are visible
		// in session history once SetHistory writes the message below.
		if err := hook(ctx, sessionKey, userMsg.Content); err != nil {
			al.emit(ctx, sessionKey, streamChan, errEvent(&HookRejectedError{Cause: err}))
			return
		}
	}

	// Per-Run cost accumulator: stashed on ctx so callLLM can bump it
	// from any iteration without threading new params. The deferred
	// emitCost fires on every terminal exit (final answer, MaxIters,
	// MaxToolCallsPerSession, fatal LLM error) instead of only on the
	// final-answer success path. Installed unconditionally so a
	// provider that prices its own calls is recorded without a table.
	var emitCost func()
	ctx, emitCost = al.installRunCostAccumulator(ctx, sessionKey, streamChan)
	defer emitCost()

	// Per-Run degradation accumulator. The final-answer path drains it
	// before DoneEvent; this deferred sweep catches Runs that degraded and
	// then ended on a cap or fatal error, where the unreliable state most
	// needs reporting. drain() makes the two mutually exclusive.
	var sweepDegraded func()
	ctx, sweepDegraded = al.installDegradationAccumulator(ctx, sessionKey, streamChan)
	defer sweepDegraded()

	// Load memory notes once per Run and stash on ctx; buildMsgsForLLM
	// reads the cached value on every iteration so notes show up on every
	// LLM call without re-hitting the Store. Persisted history is left
	// untouched — session managers that rewrite the system prompt on read
	// (e.g. InMem) would otherwise erase the injection between turns.
	//
	// Emits MemoryLoadedEvent with the resolved scope so adopters get an
	// audit signal even when zero notes load (store error, fresh scope).
	// Scope=="" means fail-closed (resolver returned "" — typically an
	// unauthenticated request); skip the event entirely in that case so
	// audit logs only record real attempts.
	if scope, notes, count := al.loadMemoryForRun(ctx, sessionKey); scope != "" {
		if notes != "" {
			ctx = withMemoryNotes(ctx, notes)
		}
		al.emit(ctx, sessionKey, streamChan, Event(MemoryLoadedEvent{
			Scope:           scope,
			NoteCount:       count,
			EstimatedTokens: len(notes) / memoryCharsPerToken,
		}))
	}

	existing, err := al.Sessions.History(ctx, sessionKey)
	if err != nil {
		al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("agent: load history: %w", err)))
		return
	}
	if isFreshSessionHistory(existing) {
		al.emit(ctx, sessionKey, streamChan, Event(SessionCreatedEvent{SessionKey: sessionKey}))
	}
	msgs := append(existing, userMsg)
	msgs = patchDanglingToolCalls(msgs)
	if err := al.Sessions.SaveHistory(ctx, sessionKey, msgs); err != nil {
		al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("agent: save history: %w", err)))
		return
	}

	al.iterateMessages(ctx, sessionKey, streamChan, msgs)
	al.fireConsolidator(ctx, sessionKey)
}

// fireConsolidator launches the post-session consolidator on a detached
// goroutine. No-op when no consolidator is configured, and no-op when
// the resolved scope is "" (fail-closed — typically an unauthenticated
// request). The goroutine uses context.WithoutCancel so it survives
// request-scoped cancellation, and re-reads the transcript from
// SessionManager (the source of truth after iterateMessages persisted
// its terminal state) rather than capturing the loop's working slice.
//
// On completion (success or failure), emits MemoryConsolidatedEvent to
// the EventHandlers chain. The stream channel is already closed by the
// time this runs, so the event reaches programmatic consumers only —
// SSE relays that want to surface consolidation events must hook the
// EventHandler API.
func (al *AgentLoop) fireConsolidator(ctx context.Context, sessionKey string) {
	if al.MemoryConsolidator == nil {
		return
	}
	scope := al.resolveMemoryScope(ctx, sessionKey)
	if scope == "" {
		return
	}
	// Apply the FirePolicy throttle BEFORE the goroutine spawn — the
	// per-scope bookkeeping (turn counter, lastFiredAt) must serialize
	// across concurrent Run completions. shouldFire stamps lastFiredAt
	// eagerly so a second Run that completes while the prior fire is
	// still running doesn't double-trigger.
	if !al.MemoryConsolidator.shouldFire(scope) {
		return
	}
	detached := context.WithoutCancel(ctx)
	al.bgWg.Add(1)
	go al.runConsolidator(detached, sessionKey, scope)
}

// runConsolidator is the detached goroutine body. Extracted so the
// closure stays small (no 5+ captures per project decomposition rules)
// and the audit emission has one obvious site to reason about.
//
// Decrements al.bgWg on exit so Shutdown can wait for in-flight work.
func (al *AgentLoop) runConsolidator(ctx context.Context, sessionKey, scope string) {
	defer al.bgWg.Done()
	transcript, herr := al.Sessions.History(ctx, sessionKey)
	if herr != nil {
		log.Printf("[gopheragent] consolidator: history read failed for %q: %v", sessionKey, herr)
		al.emitPostStream(ctx, sessionKey, Event(MemoryConsolidatedEvent{
			Scope: scope,
			Error: herr.Error(),
		}))
		return
	}
	res, err := al.MemoryConsolidator.Consolidate(ctx, scope, transcript)
	if err != nil {
		log.Printf("[gopheragent] consolidator: scope %q: %v", scope, err)
	}
	ev := MemoryConsolidatedEvent{Scope: scope, Before: res.Before, After: res.After}
	if err != nil {
		ev.Error = err.Error()
	}
	al.emitPostStream(ctx, sessionKey, Event(ev))
}

// Shutdown blocks until every background goroutine the loop launched
// (today: post-Run consolidators) has finished, or until ctx fires.
// Returns ctx.Err when the deadline expires with work still in flight.
//
// Typical use lives at the end of an HTTP server's graceful-stop path:
//
//	srv.Shutdown(ctx)            // stop accepting new requests
//	loop.Shutdown(ctxWithDeadline) // wait for detached bg work
//
// Shutdown does NOT prevent new background work from being scheduled —
// adopters that need a hard stop should stop calling Run before
// Shutdown so no new consolidators get spawned. The loop itself stays
// reusable after Shutdown returns.
//
// Calling Shutdown with no in-flight work returns nil immediately.
func (al *AgentLoop) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		al.bgWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// emitPostStream fires only the EventHandlers chain — used for events
// produced by detached goroutines after the per-Run stream channel has
// closed. Bypassing the streamChan write side is what makes this safe:
// emit() would panic on a closed channel.
func (al *AgentLoop) emitPostStream(ctx context.Context, sessionKey string, ev StreamEvent) {
	for _, h := range al.EventHandlers {
		safeCallHandler(h, ctx, sessionKey, ev)
	}
}

// iterateMessages is the shared iteration body used by every loop entry point
// (runLogicLoop, Regenerate, Continue). The caller is responsible for
// preparing msgs (PatchDanglingToolCalls applied and persisted via
// SetHistory) before invoking it. iterateMessages drives the loop to a
// terminal frame — final answer, fatal error, cap exhaustion, or
// MaxIters — and never returns without emitting one.
func (al *AgentLoop) iterateMessages(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, msgs []history.Message) {
	// Root span for the whole turn so all iteration/LLM/tool spans share one
	// trace ID; correlate turns of a conversation by the session.key attribute.
	ctx, endRun := al.startRunSpan(ctx, sessionKey)
	defer endRun()

	loopTracker := loopDetectorFromHistory(msgs)
	totalToolCalls := 0
	for iteration := 0; iteration < al.MaxIters; iteration++ {
		scheduled, done := al.runIteration(ctx, sessionKey, streamChan, &msgs, iteration, loopTracker)
		if done {
			return
		}
		totalToolCalls += scheduled
		if al.MaxToolCallsPerSession > 0 && totalToolCalls >= al.MaxToolCallsPerSession {
			al.saveSession(ctx, sessionKey, msgs)
			al.emit(ctx, sessionKey, streamChan, limitExhaustedEvent(LimitKindMaxToolCallsPerSession, al.MaxToolCallsPerSession, totalToolCalls))
			al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("%w: %d/%d", ErrMaxToolCallsPerSession, totalToolCalls, al.MaxToolCallsPerSession)))
			return
		}
	}

	al.saveSession(ctx, sessionKey, msgs)
	al.emit(ctx, sessionKey, streamChan, Event(MaxItersReachedEvent{Limit: al.MaxIters}))
	al.emit(ctx, sessionKey, streamChan, limitExhaustedEvent(LimitKindMaxIters, al.MaxIters, al.MaxIters))
	al.emit(ctx, sessionKey, streamChan, errEvent(ErrMaxIterations))
}

// limitExhaustedEvent constructs a typed LimitExhaustedEvent stream frame.
// Kept as a free function so providers (which import pkg/agent) can call it
// without going through an AgentLoop receiver.
func limitExhaustedEvent(kind LimitKind, limit, used int) StreamEvent {
	return Event(LimitExhaustedEvent{Kind: kind, Limit: limit, Used: used})
}

// LimitExhaustedStreamEvent is the public constructor for adopters or
// custom LLM providers that want to signal a cap fired. The kind should
// be one of the LimitKind constants when applicable; custom providers
// may invent their own kind strings.
func LimitExhaustedStreamEvent(kind LimitKind, limit, used int) StreamEvent {
	return limitExhaustedEvent(kind, limit, used)
}
