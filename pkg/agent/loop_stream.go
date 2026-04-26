// Package agent provides the core ReAct loop, streaming, and orchestration for LLM agents.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

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
func (al *AgentLoop) emit(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, ev StreamEvent) {
	streamChan <- ev
	for _, h := range al.EventHandlers {
		h(ctx, sessionKey, ev)
	}
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
	streamChan := make(chan StreamEvent, 100)

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
	internalChan := make(chan StreamEvent, 50)
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
		if ctx.Err() != nil {
			al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("%w: %w", ErrContextCancelled, ctx.Err())))
			al.saveSession(ctx, sessionKey, msgs)
			return
		}

		// Token budget enforcement
		if al.MaxTokenBudget > 0 {
			estToks := estimateTokens(msgs)
			thresh85 := int(float64(al.MaxTokenBudget) * 0.85)
			
			if estToks > thresh85 && estToks <= al.MaxTokenBudget {
				al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf("Token budget near threshold (~%d >= %d). Truncating tool arguments.", estToks, thresh85)})
				msgs = TruncateToolArguments(msgs)
			}
			
			if estimateTokens(msgs) > al.MaxTokenBudget {
				al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf(
					"Token budget exceeded (~%d est. tokens). Applying emergency context pruning.", estimateTokens(msgs),
				)})
				msgs = PruneContextMessages(msgs, 1)
			} else {
				msgs = PruneContextMessages(msgs, 3)
			}
		} else {
			msgs = PruneContextMessages(msgs, 3)
		}

		if iteration == al.MaxIters-2 {
			al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: "System: Nudging Agent for a soft landing before MaxIters limit is reached."})
			msgs = append(msgs, history.Message{
				Role:    "system",
				Content: "[System] You are approaching the iteration limit. Please wrap up and provide the final answer to the user immediately.",
			})
			al.Sessions.SetHistory(ctx, sessionKey, msgs)
		}

		// speculativeMap carries results for tool calls the drainer kicked
		// off mid-stream. Allocated fresh per iteration so retries never
		// see stale entries from an earlier failed attempt.
		specMap := newSpeculativeMap()
		var specMu sync.Mutex

		// callLLM taps a fresh providerChan, forwards events to streamChan, and
		// accumulates content. Returns (finalContent, result, contentWasEmitted, err).
		// Retry is safe only when contentWasEmitted == false.
		callLLM := func() (string, LLMResult, bool, error) {
			// Reset speculation state at the start of each attempt so a
			// retry's map never mixes with prior-attempt speculations.
			specMu.Lock()
			for k := range specMap {
				delete(specMap, k)
			}
			specMu.Unlock()
			// BeforeLLMHooks: any non-nil error aborts this call (no retry).
			for _, h := range al.BeforeLLMHooks {
				if h == nil {
					continue
				}
				if err := h(ctx, sessionKey, estimateTokens(msgs)); err != nil {
					return "", LLMResult{}, false, err
				}
			}

			pChan := make(chan StreamEvent, 50)
			var buf strings.Builder
			var emitted bool
			done := make(chan struct{})
			go func() {
				defer close(done)
				for ev := range pChan {
					if ev.Type == "content" {
						buf.WriteString(ev.Content)
						emitted = true
					}
					// Kick off safe tool calls the moment the provider has a
					// complete invocation. Errors in eligibility are silent —
					// the wave executor will run the tool the normal way.
					if ev.Type == EventTypeToolCallReady {
						if p, ok := ev.Payload().(ToolCallReadyEvent); ok {
							if al.shouldSpeculate(sessionKey, p.ID, p.Name, p.ArgsJSON) {
								al.spawnSpeculative(ctx, p.ID, p.Name, p.ArgsJSON, &specMu, specMap)
							}
						}
					}
					select {
					case streamChan <- ev:
						for _, h := range al.EventHandlers {
							h(ctx, sessionKey, ev)
						}
					case <-ctx.Done():
						for range pChan {
						}
						return
					}
				}
			}()
			toolsForCall := al.Tools
			if al.IsPlanMode(sessionKey) {
				toolsForCall = withPlanModeTool(toolsForCall)
			}
			if al.ToolSelector != nil {
				query := latestUserMessage(msgs)
				if filtered, selErr := al.ToolSelector.SelectRegistry(ctx, query); selErr == nil && filtered != nil {
					toolsForCall = filtered
				} else if selErr != nil {
					al.emit(ctx, sessionKey, streamChan, StreamEvent{
						Type:    "thought",
						Content: fmt.Sprintf("Tool selector error, falling back to full registry: %v", selErr),
					})
				}
			}
			msgsForLLM := al.withDynamicContext(ctx, sessionKey, al.withPlanModeHint(sessionKey, al.withToolChainingHint(msgs)))
			if al.AutoCacheSystem && len(msgsForLLM) > 0 && msgsForLLM[0].Role == "system" && !msgsForLLM[0].CacheHint {
				// Copy before stamping: msgsForLLM can alias the input slice
				// when no upstream hint needed a fresh allocation (DynamicContext
				// nil + plan-mode off + tool-chaining hint inactive). Mutating
				// the alias would leak CacheHint into the caller's session-
				// loaded slice.
				stamped := make([]history.Message, len(msgsForLLM))
				copy(stamped, msgsForLLM)
				stamped[0].CacheHint = true
				msgsForLLM = stamped
			}
			llmCtx := ctx
			if al.ThinkingBudget > 0 {
				llmCtx = WithThinkingBudget(ctx, al.ThinkingBudget)
			}
			res, err := al.LLM.GenerateStream(llmCtx, msgsForLLM, toolsForCall, pChan)
			close(pChan)
			<-done
			content := buf.String()
			if content == "" {
				content = res.Content
			}
			// Emit a usage event when the provider reported token accounting.
			if err == nil && res.Usage.TotalTokens > 0 {
				payload, _ := json.Marshal(res.Usage)
				al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeUsage, Content: string(payload)})
			}
			return content, res, emitted, err
		}

		finalContent, result, _, err := callLLM()

		// Retry on transient errors — only when no content has been streamed yet.
		if err != nil && al.Retry != nil && isRetryable(err) {
			for attempt := 0; attempt < al.Retry.MaxRetries; attempt++ {
				delay := al.Retry.delay(attempt)
				al.emit(ctx, sessionKey, streamChan, StreamEvent{
					Type:    "thought",
					Content: fmt.Sprintf("LLM error (%v). Retrying in %s (attempt %d/%d)...", err, delay, attempt+1, al.Retry.MaxRetries),
				})
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					err = ctx.Err()
					goto doneRetry
				}
				var retryContent string
				var contentEmitted bool
				retryContent, result, contentEmitted, err = callLLM()
				if retryContent != "" {
					finalContent = retryContent
				}
				if err == nil || contentEmitted || !isRetryable(err) {
					break
				}
			}
		}
	doneRetry:

		if err != nil {
			al.emit(ctx, sessionKey, streamChan, errEvent(&LLMFailureError{Cause: err}))
			al.saveSession(ctx, sessionKey, msgs)
			return
		}

		// Per-turn tool-call budget. Truncate-and-inform: first N schedule
		// normally, dropped IDs remain on the assistant message so the
		// transcript matches what the model emitted, and each dropped call
		// gets a synthesized error tool result below so the model learns
		// why they did not run.
		var droppedCalls []PendingToolCall
		scheduled := result.ToolCalls
		if al.MaxToolCallsPerTurn > 0 && len(result.ToolCalls) > al.MaxToolCallsPerTurn {
			droppedCalls = result.ToolCalls[al.MaxToolCallsPerTurn:]
			scheduled = result.ToolCalls[:al.MaxToolCallsPerTurn]
			al.emit(ctx, sessionKey, streamChan, StreamEvent{
				Type:    "thought",
				Content: fmt.Sprintf("Tool-call budget exceeded: executing first %d of %d; dropping %d.", al.MaxToolCallsPerTurn, len(result.ToolCalls), len(droppedCalls)),
			})
		}

		if len(result.ToolCalls) == 0 {
			msgs = append(msgs, history.Message{Role: "assistant", Content: finalContent})

			// Optional serial self-critique. Runs only on the terminal branch
			// so we never reflect over partial states where the model still
			// owes tool calls. Errors inside a round are surfaced as thought
			// events and break out of the loop so the original answer stands.
			if al.Reflect > 0 && finalContent != "" {
				for r := 1; r <= al.Reflect; r++ {
					al.emit(ctx, sessionKey, streamChan, StreamEvent{
						Type:    "thought",
						Content: fmt.Sprintf("Self-critique pass %d/%d...", r, al.Reflect),
					})
					revised, rerr := al.reflectOnce(ctx, sessionKey, msgs, r, streamChan)
					if rerr != nil {
						al.emit(ctx, sessionKey, streamChan, StreamEvent{
							Type:    "thought",
							Content: fmt.Sprintf("Self-critique aborted: %v", rerr),
						})
						break
					}
					if revised == "" || revised == finalContent {
						continue
					}
					finalContent = revised
					msgs[len(msgs)-1].Content = finalContent
					al.emit(ctx, sessionKey, streamChan, StreamEvent{
						Type:    EventTypeReflected,
						Content: reflectedEventContent(finalContent, r),
					})
				}
			}

			al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeDone})
			al.saveSession(ctx, sessionKey, msgs)
			return
		}

		assistantMsg := history.Message{Role: "assistant", Content: finalContent}
		for _, tc := range result.ToolCalls {
			assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, history.ToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: tc.ArgsJSON,
			})
		}
		msgs = append(msgs, assistantMsg)
		al.Sessions.SetHistory(ctx, sessionKey, msgs)

		// Schedule tool calls into dependency waves. When the LLM emits
		// <output_of:ID.path> references in ArgsJSON, calls are ordered so
		// each wave depends only on completed waves; substitution happens
		// right before a call runs. If scheduling fails (cycle, unknown ref)
		// we fall back to the legacy behavior of running all calls in
		// parallel, letting the tools themselves reject bad input.
		waves, schedErr := ScheduleToolCalls(scheduled)
		if schedErr != nil {
			al.emit(ctx, sessionKey, streamChan, StreamEvent{
				Type:    "thought",
				Content: fmt.Sprintf("Tool scheduler: %v — running all calls in one wave.", schedErr),
			})
			waves = [][]PendingToolCall{scheduled}
		}

		toolMsgs := make(map[string]history.Message, len(result.ToolCalls))
		resultsByID := make(map[string]string, len(result.ToolCalls))
		var completedMu sync.Mutex
		var fatalErr error
		var fatalMu sync.Mutex
		var hitlMu sync.Mutex

		for _, wave := range waves {
			// Substitute <output_of:...> tokens in this wave's args using
			// results from earlier waves. Resolver reads under the lock to
			// stay race-free against any residual goroutine writes.
			substitutedWave := make([]PendingToolCall, len(wave))
			for i, tc := range wave {
				completedMu.Lock()
				resolver := func(id string) (string, bool) {
					r, ok := resultsByID[id]
					return r, ok
				}
				newArgs, subErr := Substitute(tc.ArgsJSON, resolver)
				completedMu.Unlock()
				if subErr == nil {
					tc.ArgsJSON = newArgs
				} else {
					al.emit(ctx, sessionKey, streamChan, StreamEvent{
						Type:    "thought",
						Content: fmt.Sprintf("Tool scheduler: substitution for %q failed: %v", tc.Name, subErr),
					})
				}
				substitutedWave[i] = tc
			}

			var wg sync.WaitGroup
			for _, tc := range substitutedWave {
				wg.Add(1)
				go func(tCall PendingToolCall) {
					defer wg.Done()

					fatalMu.Lock()
					hasFatal := fatalErr != nil
					fatalMu.Unlock()
					if hasFatal {
						return
					}

					// Plan-mode gate runs before the tool-registry lookup so
					// exit_plan_mode can be a loop-level sentinel even when
					// the caller did not register a concrete tool for it.
					if func() bool {
						hitlMu.Lock()
						defer hitlMu.Unlock()
						if !al.IsPlanMode(sessionKey) {
							return false
						}
						if tCall.Name != ExitPlanModeToolName {
							msg := fmt.Sprintf(`{"error":"tool %q is blocked in plan mode. Present your full plan via exit_plan_mode first and wait for user approval."}`, tCall.Name)
							completedMu.Lock()
							toolMsgs[tCall.ID] = history.Message{Role: "tool", Content: msg, ToolCallID: tCall.ID, IsError: true}
							completedMu.Unlock()
							return true
						}
						var pa struct {
							Plan string `json:"plan"`
						}
						_ = json.Unmarshal([]byte(tCall.ArgsJSON), &pa)
						al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: "Plan proposed — awaiting human approval."})
						approved := false
						if al.ConfirmPlan != nil {
							approved = al.ConfirmPlan(ctx, pa.Plan)
						} else {
							payload, _ := json.Marshal(map[string]string{"tool": ExitPlanModeToolName, "plan": pa.Plan})
							al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeActionRequired, Content: string(payload)})
						}
						var result string
						isErr := false
						if approved {
							al.SetPlanMode(sessionKey, false)
							al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: "Plan approved — exiting plan mode."})
							result = `{"approved":true}`
						} else {
							result = `{"approved":false,"reason":"User rejected the plan. Revise based on their feedback and propose again via exit_plan_mode."}`
							isErr = true
						}
						completedMu.Lock()
						toolMsgs[tCall.ID] = history.Message{Role: "tool", Content: result, ToolCallID: tCall.ID, IsError: isErr}
						if !isErr {
							resultsByID[tCall.ID] = result
						}
						completedMu.Unlock()
						return true
					}() {
						return
					}

					tool, ok := al.Tools.Get(tCall.Name)
					if !ok {
						toolErr := &ToolNotFoundError{ToolName: tCall.Name}
						al.emit(ctx, sessionKey, streamChan, errEvent(toolErr))
						completedMu.Lock()
						toolMsgs[tCall.ID] = history.Message{
							Role:       "tool",
							Content:    toolErr.Error(),
							ToolCallID: tCall.ID,
							IsError:    true,
						}
						completedMu.Unlock()
						return
					}

					// Consult the permission policy before HITL. Allow bypasses
					// RequiresConfirmation(); Deny short-circuits even for tools
					// that would otherwise run without a prompt; Prompt falls
					// through to the existing HITL flow.
					permDecision := PermissionPrompt
					if al.Permissions != nil {
						permDecision = al.Permissions.Check(ctx, tCall.Name, tCall.ArgsJSON)
					}
					if permDecision == PermissionAllow && tool.RequiresConfirmation() {
						// Make it visible in the transcript that a HITL-gated
						// tool was auto-approved by policy rather than a human.
						al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf("Permission policy auto-approved %s (bypassing HITL).", tCall.Name)})
					}
					if permDecision == PermissionDeny {
						deniedErr := &PermissionDeniedError{ToolName: tCall.Name}
						al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf("Permission policy denied %s — skipping execution.", tCall.Name)})
						completedMu.Lock()
						toolMsgs[tCall.ID] = history.Message{
							Role:       "tool",
							Content:    fmt.Sprintf("%v. Do not attempt this action again. Find a workaround.", deniedErr),
							ToolCallID: tCall.ID,
							IsError:    true,
						}
						completedMu.Unlock()
						return
					}

					if tool.RequiresConfirmation() && permDecision != PermissionAllow {
						approved := func() bool {
							hitlMu.Lock()
							defer hitlMu.Unlock()

							al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: "CRITICAL: Tool requires human confirmation."})

							appr := false
							if al.ConfirmHITL != nil {
								appr = al.ConfirmHITL(ctx, tCall.Name, tCall.ArgsJSON)
							} else {
								payload, _ := json.Marshal(map[string]string{"tool": tCall.Name, "args": tCall.ArgsJSON})
								al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeActionRequired, Content: string(payload)})
							}
							return appr
						}()

						if !approved {
							deniedErr := &HITLDeniedError{ToolName: tCall.Name}
							completedMu.Lock()
							toolMsgs[tCall.ID] = history.Message{
								Role:       "tool",
								Content:    fmt.Sprintf("%v. Do not attempt this action again. Find a workaround.", deniedErr),
								ToolCallID: tCall.ID,
								IsError:    true,
							}
							completedMu.Unlock()
							return
						}
						al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: "Human APPROVED tool execution."})
					}

					cacheKey := toolCacheKey(tCall.Name, tCall.ArgsJSON)
					cacheOK := false
					if al.Cache != nil {
						if c, ok := tool.(tools.Cacheable); ok && c.Cacheable() {
							cacheOK = true
						}
					}

					// Announce the tool call before any fast-path return.
					// Cache hits and speculative reuses are still scheduled
					// calls from a consumer's perspective — they need a
					// matching tool_call event so observers (UI counters,
					// telemetry spans, MaxToolCallsPerTurn analytics) see
					// every call exactly once. Subsequent thought events
					// disambiguate the source ("Cache hit", "Reusing
					// speculative result").
					al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeToolCall, Content: fmt.Sprintf("Executing: %s", tCall.Name)})

					if cacheOK {
						if cached, hit := al.Cache.Get(cacheKey); hit {
							al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf("Cache hit for %s, skipping execution.", tCall.Name)})
							completedMu.Lock()
							toolMsgs[tCall.ID] = history.Message{Role: "tool", Content: cached, ToolCallID: tCall.ID}
							resultsByID[tCall.ID] = cached
							completedMu.Unlock()
							return
						}
					}

					toolCtx := tools.WithProgressFunc(ctx, func(msg string) {
						ev := StreamEvent{Type: EventTypeToolProgress, Content: msg}
						select {
						case streamChan <- ev:
							for _, h := range al.EventHandlers {
								h(ctx, sessionKey, ev)
							}
						default:
						}
					})
					toolCtx = WithSubAgentEmitter(toolCtx, func(ev StreamEvent) {
						select {
						case streamChan <- ev:
							for _, h := range al.EventHandlers {
								h(ctx, sessionKey, ev)
							}
						default:
						}
					})
					// If the drainer speculatively started this call mid-stream,
					// block on its result rather than re-executing. The
					// speculation is always for the exact argsJSON we have now
					// because shouldSpeculate refuses to speculate anything
					// that could later be rewritten (<output_of:...> refs).
					specMu.Lock()
					sm, speculated := specMap[tCall.ID]
					specMu.Unlock()
					var toolResult string
					var execErr error
					if speculated {
						al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf("Reusing speculative result for %s.", tCall.Name)})
						toolResult, execErr = awaitSpeculative(toolCtx, sm)
					} else {
						toolResult, execErr = tool.Execute(toolCtx, tCall.ArgsJSON)
					}
					content := toolResult
					isToolErr := execErr != nil
					if isToolErr {
						content = al.formatToolError(tCall.Name, execErr.Error())
					}

					if !isToolErr {
						if ir, ok := tool.(tools.InlineRenderer); ok && ir.InlineResult() {
							al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeContent, Content: "\n\n" + content + "\n\n"})
						}
					}

					if cacheOK && !isToolErr {
						al.Cache.Put(cacheKey, content)
					}

					loopTracker.AddCall(tCall.Name, tCall.ArgsJSON, content)
					warnMessage, loopErr := loopTracker.Detect()
					if loopErr != nil {
						fatalMu.Lock()
						if fatalErr == nil {
							fatalErr = loopErr
						}
						fatalMu.Unlock()
						return
					}
					if warnMessage != "" {
						content += "\n\n" + warnMessage
						al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: "System inserted an anti-loop warning into context window."})
					}

					completedMu.Lock()
					toolMsgs[tCall.ID] = history.Message{
						Role:       "tool",
						Content:    content,
						ToolCallID: tCall.ID,
						IsError:    isToolErr,
					}
					if !isToolErr {
						resultsByID[tCall.ID] = content
					}
					completedMu.Unlock()
				}(tc)
			}
			wg.Wait()

			if fatalErr != nil {
				break
			}
		}

		if fatalErr != nil {
			al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("%w: %w", ErrLoopDetected, fatalErr)))
			al.saveSession(ctx, sessionKey, msgs)
			return
		}

		// Synthesize tool-error results for calls dropped by the per-turn
		// budget so the drain loop below emits them in their original
		// position and the model reads a clear reason next turn.
		for _, tc := range droppedCalls {
			toolMsgs[tc.ID] = history.Message{
				Role:       "tool",
				Content:    fmt.Sprintf("tools: dropped by per-turn tool-call budget (max=%d); retry fewer calls next turn.", al.MaxToolCallsPerTurn),
				ToolCallID: tc.ID,
				IsError:    true,
			}
		}

		// Append tool results in the LLM's original ToolCall order so the
		// transcript matches what the model emitted, regardless of which
		// scheduling wave each call ran in.
		for _, tc := range result.ToolCalls {
			if m, ok := toolMsgs[tc.ID]; ok && m.Role != "" {
				msgs = append(msgs, m)
			}
		}
		al.Sessions.SetHistory(ctx, sessionKey, msgs)
	}

	al.emit(ctx, sessionKey, streamChan, errEvent(ErrMaxIterations))
	al.saveSession(ctx, sessionKey, msgs)
}
