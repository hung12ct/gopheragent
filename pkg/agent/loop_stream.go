// Package agent provides the core ReAct loop, streaming, and orchestration for LLM agents.
package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/cache"
	"github.com/hung12ct/gopheragent/pkg/history"
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

// SessionManager interface abstracts history storage.
//
// Breaking-change note: DeleteSession and Fork were added for ephemeral worker
// sessions and conversation branching respectively. Any external implementation
// of this interface must provide them.
type SessionManager interface {
	GetHistory(ctx context.Context, sessionKey string) []history.Message
	SetHistory(ctx context.Context, sessionKey string, messages []history.Message)
	GetAsyncTasks(ctx context.Context, sessionKey string) map[string]history.AsyncTask
	SetAsyncTasks(ctx context.Context, sessionKey string, tasks map[string]history.AsyncTask)
	Save(ctx context.Context, sessionKey string) error
	// DeleteSession removes all in-memory and persisted state for sessionKey.
	// Used to clean up ephemeral sub-agent and async-worker sessions after they
	// complete so they do not accumulate in storage. Deleting a non-existent
	// session is a no-op (returns nil).
	DeleteSession(ctx context.Context, sessionKey string) error
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

// PendingToolCall represents a single tool invocation requested by the LLM.
type PendingToolCall struct {
	ID       string
	Name     string
	ArgsJSON string
}

// TokenUsage carries per-call token accounting returned by an LLM provider.
// Providers that do not report usage leave the fields zero.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
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

// ToolResultHook fires after a successful tool execution and before the
// result is appended to the LLM context, the inline-render path runs, the
// cache stores the value, or the anti-loop tracker records the call. The
// returned string replaces the original result; a non-nil error converts
// the call into a tool error (formatted via formatToolError, same as if
// tool.Execute had returned the error).
//
// Typical uses: rewrite URLs (e.g. local -> CDN), redact secrets, normalize
// formats, or veto a result that fails post-validation.
//
// The hook does NOT fire when tool.Execute itself returned an error — error
// handling stays untouched. ctx is the per-tool ctx (carries WithProgressFunc
// / WithSubAgentEmitter / WithDynamicContextFunc).
type ToolResultHook func(ctx context.Context, toolName, argsJSON, result string) (string, error)

// LLMProvider abstracts the model backend (OpenAI/Gemini/Claude).
type LLMProvider interface {
	GenerateStream(ctx context.Context, memory []history.Message, availableTools *tools.Registry, streamChan chan<- StreamEvent) (LLMResult, error)
}

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
	// emitted to callers. 0 (default) disables reflection entirely.
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
}

// NewAgentLoop creates a new agent with the given session manager, tool registry, and LLM provider.
// Optional hooks run before each iteration for security/policy enforcement.
func NewAgentLoop(sessions SessionManager, registry *tools.Registry, llm LLMProvider, hooks ...Hook) *AgentLoop {
	return &AgentLoop{
		Sessions:        sessions,
		Tools:           registry,
		LLM:             llm,
		MaxIters:        15,
		EmitThoughts:    true,
		BeforeHooks:     hooks,
		AutoCacheSystem: true,
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
	Type     StreamEventType `json:"type"` // use the EventType* constants
	Content  string `json:"content,omitempty"`
	// Name is the bare tool name on EventTypeToolCall events. Consumers
	// should prefer this over parsing Content. Empty for non-tool events.
	Name     string `json:"name,omitempty"`
	Source   string `json:"source,omitempty"`
	ParentID string `json:"parent_id,omitempty"`
	Err      error  `json:"-"`
}

// errEvent is a convenience constructor for error StreamEvents with a typed error.
func errEvent(err error) StreamEvent {
	return StreamEvent{Type: EventTypeError, Content: err.Error(), Err: err}
}

// RunIteration provides a fast, blocking response interface (non-streaming).
// Thoughts are always suppressed — only final content and errors are returned.
func (al *AgentLoop) RunIteration(ctx context.Context, sessionKey string, userInput string) (string, error) {
	streamChan := make(chan StreamEvent, runIterationBuffer)

	go al.runLogicLoop(ctx, sessionKey, userInput, streamChan)

	var buf strings.Builder
	var lastErr error
	for ev := range streamChan {
		// Events forwarded from sub-agents (Source != "") are observational only;
		// they must not be treated as the parent's own final answer or error.
		if ev.Source != "" {
			continue
		}
		switch ev.Type {
		case "content":
			buf.WriteString(ev.Content)
		case EventTypeReflected:
			// A reflection round produced a canonical revised answer — it
			// replaces whatever Source="" content was streamed earlier this
			// iteration so RunIteration always returns the final post-critique
			// text.
			if p, ok := ev.Payload().(ReflectedEvent); ok && p.Text != "" {
				buf.Reset()
				buf.WriteString(p.Text)
			}
		case "error":
			if ev.Err != nil {
				lastErr = ev.Err
			} else {
				lastErr = fmt.Errorf("agent: %s", ev.Content)
			}
		}
	}
	return buf.String(), lastErr
}

// RunIterationStream is a streaming version of the LLM loop that pushes data to a channel.
// It supports SSE by streaming thoughts and response chunks.
func (al *AgentLoop) RunIterationStream(ctx context.Context, sessionKey string, userInput string, streamChan chan<- StreamEvent) {
	internalChan := make(chan StreamEvent, runIterationStreamBuffer)
	emitThoughts := al.EmitThoughts

	go func() {
		defer close(streamChan)
		for ev := range internalChan {
			if ev.Type == "thought" && !emitThoughts {
				continue
			}
			select {
			case streamChan <- ev:
			case <-ctx.Done():
				// Consumer disconnected — drain internalChan to unblock runLogicLoop
				for range internalChan {
				}
				return
			}
		}
	}()

	go al.runLogicLoop(ctx, sessionKey, userInput, internalChan)
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

// saveSession persists history, using background context if the request context is already cancelled.
func (al *AgentLoop) saveSession(_ context.Context, sessionKey string, msgs []history.Message) {
	// Always use Background context — session persistence must outlive the HTTP request.
	saveCtx := context.Background()
	al.Sessions.SetHistory(saveCtx, sessionKey, msgs)
	if err := al.Sessions.Save(saveCtx, sessionKey); err != nil {
		log.Printf("[gopheragent] session save error for %q: %v", sessionKey, err)
	}
}

// SessionKey string is the type used to inject sessionKey into context
type SessionKeyCtx string

// runLogicLoop holds the actual execution context separated from the stream proxy
func (al *AgentLoop) runLogicLoop(ctx context.Context, sessionKey string, userInput string, streamChan chan<- StreamEvent) {
	defer close(streamChan)
	ctx = context.WithValue(ctx, SessionKeyCtx("sessionKey"), sessionKey)

	loopTracker := NewLoopDetector()

	for _, hook := range al.BeforeHooks {
		if hook == nil {
			continue
		}
		if err := hook(ctx, sessionKey, userInput); err != nil {
			al.emit(ctx, sessionKey, streamChan, errEvent(&HookRejectedError{Cause: err}))
			return
		}
	}

	msgs := append(al.Sessions.GetHistory(ctx, sessionKey), history.Message{Role: "user", Content: userInput})
	msgs = PatchDanglingToolCalls(msgs)
	al.Sessions.SetHistory(ctx, sessionKey, msgs)

	for iteration := 0; iteration < al.MaxIters; iteration++ {
		if al.runIteration(ctx, sessionKey, streamChan, &msgs, iteration, loopTracker) {
			return
		}
	}

	al.emit(ctx, sessionKey, streamChan, errEvent(ErrMaxIterations))
	al.saveSession(ctx, sessionKey, msgs)
}
