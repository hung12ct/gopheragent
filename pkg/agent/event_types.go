package agent

import (
	"encoding/json"
	"fmt"
	"time"
)

// StreamEventType tags the kind of event carried by StreamEvent. It is a
// distinct string type so the compiler catches bare-string comparisons and
// emits against non-constant strings — use the EventType* constants below
// for every emit and match.
type StreamEventType string

// Event type constants. Use these instead of string literals when emitting or
// matching events, so the compiler flags typos and renames stay consistent.
const (
	EventTypeContent        StreamEventType = "content"
	EventTypeThought        StreamEventType = "thought"
	EventTypeToolCall       StreamEventType = "tool_call"
	EventTypeToolProgress   StreamEventType = "tool_progress"
	EventTypeActionRequired StreamEventType = "action_required"
	EventTypeUsage          StreamEventType = "usage"
	EventTypeError          StreamEventType = "error"
	EventTypeDone           StreamEventType = "done"
	// EventTypeReflected carries the canonical answer produced by a
	// self-critique round. Streaming consumers should treat it as a
	// replacement for any prior Source="" content that fell in the same
	// iteration; RunIteration does exactly this by resetting its buffer.
	EventTypeReflected StreamEventType = "reflected"
	// EventTypeToolCallReady is emitted mid-stream by providers that
	// surface a complete tool invocation before the overall response
	// finishes streaming.
	EventTypeToolCallReady StreamEventType = "tool_call_ready"
	// EventTypeTaskList carries the current per-session task list. Consumers
	// re-render the full list on every event — there is no diff format.
	EventTypeTaskList StreamEventType = "task_list"
	// EventTypeMaxItersReached signals that the loop exhausted MaxIters
	// without a final answer. Emitted right before the legacy error event
	// carrying ErrMaxIterations so callers using errors.Is keep working;
	// SSE consumers should prefer this typed signal to distinguish "hit
	// the cap" from generic errors.
	EventTypeMaxItersReached StreamEventType = "max_iters_reached"
	// EventTypeSessionCreated is emitted as the first frame of a stream
	// when the loop is starting a previously-unseen session.
	EventTypeSessionCreated StreamEventType = "session_created"
	// EventTypeLimitExhausted is the canonical signal that a configured
	// cap fired. Prefer it to the legacy error event for new consumers.
	EventTypeLimitExhausted StreamEventType = "limit_exhausted"
	// EventTypeHITLDenied is emitted when the HITL gate resolves to "user
	// denied". Observational; the per-tool denial directive is already
	// recorded as a tool message by the loop.
	EventTypeHITLDenied StreamEventType = "hitl_denied"
	// EventTypeHITLTimedOut is emitted when AgentLoop.ConfirmHITLTimeout
	// fires before the operator responds. Distinct from EventTypeHITLDenied
	// so consumers can route differently.
	EventTypeHITLTimedOut StreamEventType = "hitl_timed_out"
	// EventTypeRegenerated marks the start of an AgentLoop.Regenerate replay.
	EventTypeRegenerated StreamEventType = "regenerated"
	// EventTypeContinued marks the start of an AgentLoop.Continue resume.
	EventTypeContinued StreamEventType = "continued"
	// EventTypeMemoryLoaded is emitted at the start of every Run that
	// has memory enabled, carrying the resolved scope and the count /
	// estimated token cost of the notes injected. Use it for audit
	// logs ("agent loaded N notes for scope user:alice") and to
	// detect cross-tenant anomalies after the fact. Emitted exactly
	// once per Run; skipped when memory is disabled or the scope
	// resolver returned "" (fail-closed path).
	EventTypeMemoryLoaded StreamEventType = "memory_loaded"
	// EventTypeMemoryConsolidated is emitted from the detached
	// consolidator goroutine after Consolidate returns, regardless of
	// outcome. Carries Before/After counts and any error so adopters
	// can wire a compliance log without polling the store.
	EventTypeMemoryConsolidated StreamEventType = "memory_consolidated"
	// EventTypeRunCost is emitted right before DoneEvent on Runs that
	// produced a final answer. Carries rolled-up token usage and the
	// computed dollar cost under the configured PriceTable. Skipped
	// when no PriceTable is configured (zero-cost when unused).
	EventTypeRunCost StreamEventType = "run_cost"
	// EventTypeContextTrace records what the pruner rewrote on the way
	// into one LLM call. Emitted only when a prune actually changed
	// something, so a turn whose context always fits stays silent.
	EventTypeContextTrace StreamEventType = "context_trace"
	// EventTypeDegraded reports that the turn finished with some tool's
	// derived bookkeeping left inconsistent, even though the turn itself
	// produced a real answer. Precedes the terminal event, never replaces
	// it — a consumer that ignores it sees exactly today's behavior.
	EventTypeDegraded StreamEventType = "degraded"
)

// LimitKind enumerates the cap categories surfaced via LimitExhaustedEvent.
// Custom providers may invent their own kinds — adopters that match
// exhaustively should use a default branch.
type LimitKind string

const (
	// LimitKindMaxIters: AgentLoop.MaxIters reached without a final answer.
	LimitKindMaxIters LimitKind = "max_iters"
	// LimitKindMaxToolCallsPerSession: cumulative tool-call cap exceeded.
	LimitKindMaxToolCallsPerSession LimitKind = "max_tool_calls_per_session"
	// LimitKindProviderMaxTokens: the LLM provider truncated the response
	// because its per-call MaxTokens cap fired.
	LimitKindProviderMaxTokens LimitKind = "provider_max_tokens"
)

// LimitReason is an optional sub-classification for LimitExhaustedEvent.
// Empty reason = "no further detail" (clean truncation, plain cap trip).
// Anthropic's docs call out the incomplete-tool-use case as one that adopters
// should handle differently: a clean text truncation can be presented as
// "model cut off; continue?", but a truncation that ate a tool_use mid-JSON
// strands the turn (the model wanted to call a tool but never finished naming
// the arguments) — auto-retry with a larger MaxTokens is the documented
// recovery. Custom providers may invent their own reasons.
type LimitReason string

const (
	// LimitReasonIncompleteToolUse: the provider truncated the stream
	// mid-tool_use, leaving a partial JSON Input that the SDK could not
	// finalize. Adopters should retry the request with a larger MaxTokens
	// rather than treat the response as complete. Currently emitted by the
	// Anthropic provider only.
	LimitReasonIncompleteToolUse LimitReason = "incomplete_tool_use"
)

// EventPayload is a sealed interface implemented by every typed event payload.
// Every payload reports its matching StreamEventType so the Event() helper
// can build a self-consistent StreamEvent in one call. Use a type switch on
// the Payload field of a StreamEvent to pattern-match concrete cases:
//
//	switch p := ev.Payload.(type) {
//	case ContentEvent:
//	    buf.WriteString(p.Text)
//	case UsageEvent:
//	    total += p.Usage.TotalTokens
//	case ErrorEvent:
//	    return p.Err
//	}
//
// The sealing method is unexported, so only this package can add payload
// types — exhaustive matches stay stable across versions.
type EventPayload interface {
	isEventPayload()
	eventType() StreamEventType
}

// Event constructs a StreamEvent for payload p, deriving the Type field from
// the payload's static type. Source and ParentID default to empty; mutate the
// returned value if the event needs sub-agent tagging.
func Event(p EventPayload) StreamEvent {
	return StreamEvent{Type: p.eventType(), Payload: p}
}

// ContentEvent is assistant-produced text shown to the user.
type ContentEvent struct {
	Text string `json:"text"`
}

func (ContentEvent) isEventPayload()            {}
func (ContentEvent) eventType() StreamEventType { return EventTypeContent }

// ThoughtEvent is internal reasoning / system narration. Suppressed from the
// final answer by RunIteration; surfaced by RunIterationStream when
// EmitThoughts is true.
type ThoughtEvent struct {
	Message string `json:"message"`
}

func (ThoughtEvent) isEventPayload()            {}
func (ThoughtEvent) eventType() StreamEventType { return EventTypeThought }

// ToolCallEvent announces that the agent is about to execute a tool. ID is
// the agent-generated correlation ID — it matches the toolCallID parameter on
// ToolResultHook so observability tooling can pair entry and exit events
// reliably even when SpeculativeTools=true interleaves parallel calls.
// Reused is true when the wave executor consumes a speculative result
// instead of dispatching the tool again.
type ToolCallEvent struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ArgsJSON string `json:"args,omitempty"`
	Reused   bool   `json:"reused,omitempty"`
}

func (ToolCallEvent) isEventPayload()            {}
func (ToolCallEvent) eventType() StreamEventType { return EventTypeToolCall }

// ToolProgressEvent is a mid-execution status update emitted by a tool via
// tools.ReportProgress. Progress is lossy by design — consumers may drop
// these safely.
type ToolProgressEvent struct {
	Name       string `json:"name"`
	ToolCallID string `json:"tool_call_id"`
	Message    string `json:"message"`
}

func (ToolProgressEvent) isEventPayload()            {}
func (ToolProgressEvent) eventType() StreamEventType { return EventTypeToolProgress }

// ActionRequiredEvent signals that a tool invocation needs human approval.
// Adopters typically gate this through an out-of-band channel (UI prompt,
// Slack message, webhook) and resume the loop only after the operator
// responds via the configured ConfirmHITL callback.
type ActionRequiredEvent struct {
	Tool string `json:"tool"`
	Args string `json:"args,omitempty"`
}

func (ActionRequiredEvent) isEventPayload()            {}
func (ActionRequiredEvent) eventType() StreamEventType { return EventTypeActionRequired }

// UsageEvent reports token accounting from the most recent LLM call.
type UsageEvent struct {
	Usage TokenUsage `json:"usage"`
}

func (UsageEvent) isEventPayload()            {}
func (UsageEvent) eventType() StreamEventType { return EventTypeUsage }

// ErrorEvent signals a terminal failure for the current iteration. Err holds
// the structured error (usable with errors.Is / errors.As); Message is its
// string form for callers that only need display text.
type ErrorEvent struct {
	Err     error  `json:"-"`
	Message string `json:"message"`
}

func (ErrorEvent) isEventPayload()            {}
func (ErrorEvent) eventType() StreamEventType { return EventTypeError }

// DoneEvent marks the end of the stream — no more events will arrive.
type DoneEvent struct{}

func (DoneEvent) isEventPayload()            {}
func (DoneEvent) eventType() StreamEventType { return EventTypeDone }

// ReflectedEvent delivers a post-critique canonical answer. Round indicates
// which self-critique pass produced it (1-indexed); consumers typically keep
// the last seen payload as the authoritative response.
//
// The event fires only for a round the loop actually adopted, so the
// last-seen-wins contract holds whether or not AgentLoop.Scorer is set.
// Score is the adopted round's rank under that Scorer, and is nil when no
// Scorer is configured — a rejected round emits a thought event naming
// its score instead.
//
// It is a pointer because zero is a legitimate score: a 0–100 rubric can
// return 0, and the Scorer docs offer negated latency as a valid unit
// where 0 is the best possible value. A bare float64 with omitempty would
// erase that score from the wire and make it indistinguishable from
// "unscored" in Go.
type ReflectedEvent struct {
	Text  string   `json:"text"`
	Round int      `json:"round"`
	Score *float64 `json:"score,omitempty"`
}

func (ReflectedEvent) isEventPayload()            {}
func (ReflectedEvent) eventType() StreamEventType { return EventTypeReflected }

// ToolCallReadyEvent announces that a tool invocation has been fully parsed
// from the LLM stream and is eligible for execution even though the overall
// response is still streaming. AgentLoop.SpeculativeTools turns this signal
// into actual parallel execution for safe calls.
type ToolCallReadyEvent struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ArgsJSON string `json:"args,omitempty"`
}

func (ToolCallReadyEvent) isEventPayload()            {}
func (ToolCallReadyEvent) eventType() StreamEventType { return EventTypeToolCallReady }

// TaskListItem is the wire shape of a single task entry inside a
// TaskListEvent. Mirrors builtin.Task minus timestamps so consumers do not
// need to import the builtin package to render the list.
type TaskListItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"` // pending | in_progress | completed
	Notes  string `json:"notes,omitempty"`
}

// TaskListEvent is a snapshot of the session's task list emitted whenever
// a task tool mutates the list. The frontend re-renders the full list on
// every event; there is no diff format.
type TaskListEvent struct {
	Tasks []TaskListItem `json:"tasks"`
}

func (TaskListEvent) isEventPayload()            {}
func (TaskListEvent) eventType() StreamEventType { return EventTypeTaskList }

// MaxItersReachedEvent signals that the loop exhausted its iteration cap
// without a final answer. Limit echoes AgentLoop.MaxIters at the time of
// the failure.
type MaxItersReachedEvent struct {
	Limit int `json:"limit"`
}

func (MaxItersReachedEvent) isEventPayload()            {}
func (MaxItersReachedEvent) eventType() StreamEventType { return EventTypeMaxItersReached }

// SessionCreatedEvent is the first frame of a stream when the loop is
// running a never-before-seen session. Carries the session key so SSE
// relays can forward the signal to the FE for sidebar pinning, and so
// integrators can hang one-shot work (auto-title, welcome payload,
// budget allocation) off the event without racing on history-emptiness.
type SessionCreatedEvent struct {
	SessionKey string `json:"session_key"`
}

func (SessionCreatedEvent) isEventPayload()            {}
func (SessionCreatedEvent) eventType() StreamEventType { return EventTypeSessionCreated }

// LimitExhaustedEvent is the typed payload of EventTypeLimitExhausted.
// Kind identifies which cap fired (see LimitKind constants); Limit is the
// configured ceiling and Used is the actual count at the moment of the
// trip. For provider truncation kinds, Used is unset (the provider does
// not report token usage in the same shape).
//
// Reason is an optional sub-classification — currently used by the Anthropic
// provider to flag LimitReasonIncompleteToolUse so adopters can distinguish
// "model cut off cleanly" from "model truncated mid-tool_use, retry required"
// without inspecting the LLMResult themselves. Zero value = no further detail.
type LimitExhaustedEvent struct {
	Kind   LimitKind   `json:"kind"`
	Limit  int         `json:"limit"`
	Used   int         `json:"used,omitempty"`
	Reason LimitReason `json:"reason,omitempty"`
}

func (LimitExhaustedEvent) isEventPayload()            {}
func (LimitExhaustedEvent) eventType() StreamEventType { return EventTypeLimitExhausted }

// HITLDeniedEvent is the typed payload of EventTypeHITLDenied. Sub-agent
// wrappers watch for this so they can surface a clear "HITL_BLOCKED: denied"
// signal to the outer agent.
type HITLDeniedEvent struct {
	Tool string `json:"tool"`
	Args string `json:"args,omitempty"`
}

func (HITLDeniedEvent) isEventPayload()            {}
func (HITLDeniedEvent) eventType() StreamEventType { return EventTypeHITLDenied }

// MemoryLoadedEvent is the typed payload of EventTypeMemoryLoaded.
// Emitted at the start of every Run with memory enabled — adopters
// use it to log audit lines, drive UI badges, or detect anomalies
// (e.g. "agent loaded 0 notes for an authenticated user" → likely a
// scope-resolution bug).
//
// Scope is the resolved memory scope (the output of MemoryScopeFunc).
// Empty Scope means the resolver returned "" and memory was skipped
// fail-closed; the event still fires so audit consumers can record
// the attempt. NoteCount is the number of notes the loader actually
// injected (post-Limit, post-TokenBudget trim). EstimatedTokens is
// the 4-chars/token estimate of the formatted memory block.
type MemoryLoadedEvent struct {
	Scope           string `json:"scope"`
	NoteCount       int    `json:"note_count"`
	EstimatedTokens int    `json:"estimated_tokens,omitempty"`
}

func (MemoryLoadedEvent) isEventPayload()            {}
func (MemoryLoadedEvent) eventType() StreamEventType { return EventTypeMemoryLoaded }

// MemoryConsolidatedEvent is the typed payload of
// EventTypeMemoryConsolidated. Emitted by the detached consolidator
// goroutine after Consolidate returns — success or failure — so
// adopters can audit memory writes without polling the store.
//
// Before/After are the scope's note counts pre and post merge.
// Error is the consolidator's failure string when non-empty; empty
// means the consolidation succeeded (or was a no-op due to a short
// transcript). Scope is empty when fail-closed (resolver returned
// ""), in which case no consolidation ran.
type MemoryConsolidatedEvent struct {
	Scope  string `json:"scope"`
	Before int    `json:"before"`
	After  int    `json:"after"`
	Error  string `json:"error,omitempty"`
}

func (MemoryConsolidatedEvent) isEventPayload()            {}
func (MemoryConsolidatedEvent) eventType() StreamEventType { return EventTypeMemoryConsolidated }

// RunCostEvent is the typed payload of EventTypeRunCost. Emitted at
// the end of a successful Run when PriceTable is configured. Adopters
// surface USD on the UI, push to billing telemetry, or alert on
// per-Run cost spikes without re-aggregating UsageEvents themselves.
//
// USD is computed from the rolled-up Usage under the AgentLoop's
// configured Model + PriceTable. When the Model isn't in the table,
// USD is zero but Usage still reflects the true accumulated tokens —
// useful for adopters who want to compute cost themselves with a
// dynamic price source.
type RunCostEvent struct {
	Model string     `json:"model,omitempty"`
	Usage TokenUsage `json:"usage"`
	USD   float64    `json:"usd"`
}

func (RunCostEvent) isEventPayload()            {}
func (RunCostEvent) eventType() StreamEventType { return EventTypeRunCost }

// ContextTraceEvent is the typed payload of EventTypeContextTrace. It is
// the answer to "why did the agent forget what I told it earlier" —
// context pruning runs before every LLM call and, without this, leaves no
// artifact behind.
//
// Emitted once per LLM call, and only when the pruner actually rewrote a
// message: a turn whose context comfortably fits produces no events at
// all. Changes lists every rewritten message with its reason, grouped by
// the pass that produced it — argument truncation first, then depth
// pruning — so Index is ascending within a group but not across the
// whole slice. A given Index appears at most once under the current
// thresholds
// (argument truncation clips to well under the soft-trim threshold, and
// only tool messages are eligible for both) — treat that as an
// observation, not a guarantee, and do not key a map on Index.
//
// Because the loop prunes a transient copy and leaves session history at
// full fidelity, the same long tool result is re-trimmed and re-reported
// on every iteration of a turn. Consumers that log these should expect
// near-duplicates across a multi-iteration run.
//
// EstTokensBefore/After are whole-conversation estimates that include
// tool-call arguments, so they will not equal the sum of the per-Change
// numbers, which cover message content only. Both use the 4-chars/token
// heuristic MaxTokenBudget enforcement uses — good for spotting how much
// a prune saved, not for billing.
//
// Iteration is the 0-indexed loop iteration the prune fed.
type ContextTraceEvent struct {
	Policy          ContextPolicy `json:"policy"`
	Iteration       int           `json:"iteration"`
	Changes         []ContextRef  `json:"changes"`
	EstTokensBefore int           `json:"est_tokens_before"`
	EstTokensAfter  int           `json:"est_tokens_after"`
}

func (ContextTraceEvent) isEventPayload()            {}
func (ContextTraceEvent) eventType() StreamEventType { return EventTypeContextTrace }

// DegradedEvent is the typed payload of EventTypeDegraded — the terminal
// state that had no representation before: the expensive artifact landed,
// the derived bookkeeping did not. Reporting it as Done would hide a real
// inconsistency; reporting it as an error would invite a retry that
// duplicates work that already succeeded.
//
// Fires only when at least one tool returned a tools.Degradation, and
// annotates the turn's terminal rather than replacing it, so existing
// consumers are unaffected. Position depends on how the turn ended:
//
//   - Final answer: emitted immediately BEFORE DoneEvent.
//   - Cap or fatal error (MaxIters, tool-call cap, LLM failure): emitted
//     by a deferred Run-level sweep, so it arrives AFTER the
//     LimitExhaustedEvent / ErrorEvent frame.
//
// The second case matters: a consumer that tears down on the first error
// frame will miss it. Drain the stream to completion if you need the
// degradation record from failed turns — which is exactly the turn whose
// unreliable state most needs repairing.
//
// Units lists every degradation of the Run in the order the tools raised
// them. Err carries the same information as a *DegradedError for adopters
// that classify by errors.Is(err, ErrDegraded); it is reconstructed from
// Units when the event is decoded from the wire.
type DegradedEvent struct {
	Units []ToolDegradation `json:"units"`
	Err   error             `json:"-"`
}

func (DegradedEvent) isEventPayload()            {}
func (DegradedEvent) eventType() StreamEventType { return EventTypeDegraded }

// HITLTimedOutEvent is the typed payload of EventTypeHITLTimedOut. Mirrors
// HITLDeniedEvent and additionally carries the configured Timeout so a UI
// can show "approval expired after 2m" without reaching back into agent
// state.
type HITLTimedOutEvent struct {
	Tool    string        `json:"tool"`
	Args    string        `json:"args,omitempty"`
	Timeout time.Duration `json:"timeout"`
}

func (HITLTimedOutEvent) isEventPayload()            {}
func (HITLTimedOutEvent) eventType() StreamEventType { return EventTypeHITLTimedOut }

// RegeneratedEvent is the typed payload of EventTypeRegenerated.
// PreviousAssistantIndex is the index of the final assistant message in the
// pre-regenerate history — UIs mark that bubble as superseded. TruncatedAt is
// the length of the rewound prefix (msgs[:TruncatedAt] is what survived the
// safe-boundary truncation).
type RegeneratedEvent struct {
	PreviousAssistantIndex int `json:"previous_assistant_index"`
	TruncatedAt            int `json:"truncated_at"`
}

func (RegeneratedEvent) isEventPayload()            {}
func (RegeneratedEvent) eventType() StreamEventType { return EventTypeRegenerated }

// ContinuedEvent is the typed payload of EventTypeContinued.
// ContinuedFromIndex is the index of the last persisted message at the moment
// Continue was invoked.
type ContinuedEvent struct {
	ContinuedFromIndex int `json:"continued_from_index"`
}

func (ContinuedEvent) isEventPayload()            {}
func (ContinuedEvent) eventType() StreamEventType { return EventTypeContinued }

// UnknownEvent wraps an event whose Type was not recognized. It preserves
// the raw wire bytes so forward-compatible consumers can still inspect
// events produced by a newer version of the framework.
type UnknownEvent struct {
	OriginalType StreamEventType `json:"type"`
	RawJSON      string          `json:"raw"`
}

func (UnknownEvent) isEventPayload()              {}
func (u UnknownEvent) eventType() StreamEventType { return u.OriginalType }

// decodePayload constructs a typed payload from a JSON-encoded blob and the
// declared event type. Errors during inner decoding fall through to an
// UnknownEvent carrying the raw bytes, so adopters using a forward-compatible
// reader never crash on an unfamiliar type.
func decodePayload(t StreamEventType, raw []byte) EventPayload {
	switch t {
	case EventTypeContent:
		var p ContentEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeThought:
		var p ThoughtEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeToolCall:
		var p ToolCallEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeToolProgress:
		var p ToolProgressEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeActionRequired:
		var p ActionRequiredEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeUsage:
		var p UsageEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeError:
		var p ErrorEvent
		_ = json.Unmarshal(raw, &p)
		if p.Err == nil && p.Message != "" {
			p.Err = fmt.Errorf("agent: %s", p.Message)
		}
		return p
	case EventTypeDone:
		return DoneEvent{}
	case EventTypeReflected:
		var p ReflectedEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeToolCallReady:
		var p ToolCallReadyEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeTaskList:
		var p TaskListEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeMaxItersReached:
		var p MaxItersReachedEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeSessionCreated:
		var p SessionCreatedEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeLimitExhausted:
		var p LimitExhaustedEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeHITLDenied:
		var p HITLDeniedEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeHITLTimedOut:
		var p HITLTimedOutEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeRegenerated:
		var p RegeneratedEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeContinued:
		var p ContinuedEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeMemoryLoaded:
		var p MemoryLoadedEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeMemoryConsolidated:
		var p MemoryConsolidatedEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeRunCost:
		var p RunCostEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeContextTrace:
		var p ContextTraceEvent
		_ = json.Unmarshal(raw, &p)
		return p
	case EventTypeDegraded:
		var p DegradedEvent
		_ = json.Unmarshal(raw, &p)
		if p.Err == nil && len(p.Units) > 0 {
			p.Err = &DegradedError{Units: p.Units}
		}
		return p
	default:
		return UnknownEvent{OriginalType: t, RawJSON: string(raw)}
	}
}

// EventVisitor is a typed double-dispatch interface. Implementing it gives
// the compiler teeth: adding a new payload type forces every visitor in the
// codebase to handle it.
//
// Prefer the type-switch on ev.Payload for ad-hoc consumers; prefer a visitor
// when the same handler logic lives across many call sites and you want
// compiler-enforced exhaustiveness.
type EventVisitor interface {
	VisitContent(ContentEvent)
	VisitThought(ThoughtEvent)
	VisitToolCall(ToolCallEvent)
	VisitToolProgress(ToolProgressEvent)
	VisitActionRequired(ActionRequiredEvent)
	VisitUsage(UsageEvent)
	VisitError(ErrorEvent)
	VisitDone(DoneEvent)
	VisitReflected(ReflectedEvent)
	VisitToolCallReady(ToolCallReadyEvent)
	VisitTaskList(TaskListEvent)
	VisitMaxItersReached(MaxItersReachedEvent)
	VisitSessionCreated(SessionCreatedEvent)
	VisitLimitExhausted(LimitExhaustedEvent)
	VisitHITLDenied(HITLDeniedEvent)
	VisitHITLTimedOut(HITLTimedOutEvent)
	VisitRegenerated(RegeneratedEvent)
	VisitContinued(ContinuedEvent)
	VisitMemoryLoaded(MemoryLoadedEvent)
	VisitMemoryConsolidated(MemoryConsolidatedEvent)
	VisitRunCost(RunCostEvent)
	VisitContextTrace(ContextTraceEvent)
	VisitDegraded(DegradedEvent)
	VisitUnknown(UnknownEvent)
}

// Visit dispatches ev to the correct method on v based on the concrete
// payload type. Zero allocations — Payload is already typed.
func (ev StreamEvent) Visit(v EventVisitor) {
	switch p := ev.Payload.(type) {
	case ContentEvent:
		v.VisitContent(p)
	case ThoughtEvent:
		v.VisitThought(p)
	case ToolCallEvent:
		v.VisitToolCall(p)
	case ToolProgressEvent:
		v.VisitToolProgress(p)
	case ActionRequiredEvent:
		v.VisitActionRequired(p)
	case UsageEvent:
		v.VisitUsage(p)
	case ErrorEvent:
		v.VisitError(p)
	case DoneEvent:
		v.VisitDone(p)
	case ReflectedEvent:
		v.VisitReflected(p)
	case ToolCallReadyEvent:
		v.VisitToolCallReady(p)
	case TaskListEvent:
		v.VisitTaskList(p)
	case MaxItersReachedEvent:
		v.VisitMaxItersReached(p)
	case SessionCreatedEvent:
		v.VisitSessionCreated(p)
	case LimitExhaustedEvent:
		v.VisitLimitExhausted(p)
	case HITLDeniedEvent:
		v.VisitHITLDenied(p)
	case HITLTimedOutEvent:
		v.VisitHITLTimedOut(p)
	case RegeneratedEvent:
		v.VisitRegenerated(p)
	case ContinuedEvent:
		v.VisitContinued(p)
	case MemoryLoadedEvent:
		v.VisitMemoryLoaded(p)
	case MemoryConsolidatedEvent:
		v.VisitMemoryConsolidated(p)
	case RunCostEvent:
		v.VisitRunCost(p)
	case ContextTraceEvent:
		v.VisitContextTrace(p)
	case DegradedEvent:
		v.VisitDegraded(p)
	case UnknownEvent:
		v.VisitUnknown(p)
	default:
		v.VisitUnknown(UnknownEvent{OriginalType: ev.Type})
	}
}
