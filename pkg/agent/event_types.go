package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// parseExecutingName recovers the tool name from the legacy
// "Executing: <name>" Content shape used by older emitters. New emitters
// populate StreamEvent.Name directly; this is the back-compat fallback for
// events that were serialized before the Name field existed.
func parseExecutingName(content string) string {
	if rest, ok := strings.CutPrefix(content, "Executing: "); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}

// StreamEventType tags the kind of event carried by StreamEvent. It is a
// distinct string type so the compiler catches bare-string comparisons and
// emits against non-constant strings — use the EventType* constants below
// for every emit and match.
//
// Wire format is unchanged from earlier versions: StreamEventType marshals
// as a plain JSON string, so existing consumers continue to parse events
// byte-identically.
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
	// finishes streaming. It carries the fully-parsed {id,name,args} so
	// the agent loop can start executing safe tools in parallel with the
	// remaining stream, shaving one tail-latency round trip per tool.
	EventTypeToolCallReady StreamEventType = "tool_call_ready"
	// EventTypeTaskList carries the current per-session task list as JSON
	// (e.g. produced by builtin task tools). Consumers re-render the full
	// list on every event — there is no diff format. Content is the JSON
	// serialisation of TaskListEvent.Tasks.
	EventTypeTaskList StreamEventType = "task_list"
	// EventTypeMaxItersReached signals that the loop exhausted MaxIters
	// without a final answer. Emitted right before the legacy error event
	// carrying ErrMaxIterations so callers using errors.Is keep working;
	// SSE consumers should prefer this typed signal to distinguish "hit
	// the cap" from generic errors.
	EventTypeMaxItersReached StreamEventType = "max_iters_reached"
	// EventTypeSessionCreated is emitted as the first frame of a stream
	// when the loop is starting a previously-unseen session (history is a
	// single system message). One-shot work — auto-titling, welcome
	// payloads, budget allocation, analytics — should hang off this event
	// instead of racing on history-empty checks.
	EventTypeSessionCreated StreamEventType = "session_created"
	// EventTypeLimitExhausted is the canonical signal that a configured
	// cap fired — iteration cap, cumulative tool-call cap, provider
	// max_tokens, etc. Adopters can render a user-friendly message based
	// on Kind without parsing error strings. See LimitKind constants for
	// the values currently emitted by the loop and built-in providers.
	// MaxItersReachedEvent stays alongside this for back-compat but new
	// consumers should prefer LimitExhaustedEvent.
	EventTypeLimitExhausted StreamEventType = "limit_exhausted"
	// EventTypeHITLDenied is emitted when the HITL gate resolves to "user
	// denied" — the ConfirmHITL callback returned false without a timeout
	// firing. Observational; the per-tool denial directive is already
	// recorded as a tool message by the loop. Sub-agent wrappers watch for
	// this so they can surface a clear "HITL_BLOCKED: denied" signal to the
	// outer agent instead of relying on the inner agent's paraphrased
	// summary. Payload: HITLDeniedEvent.
	EventTypeHITLDenied StreamEventType = "hitl_denied"
	// EventTypeHITLTimedOut is emitted when AgentLoop.ConfirmHITLTimeout
	// fires before the operator responds. Distinct from EventTypeHITLDenied
	// so sub-agent wrappers and UI layers can route differently — typically
	// "ask the user to retry when ready" vs. "the user said no." Payload:
	// HITLTimedOutEvent.
	EventTypeHITLTimedOut StreamEventType = "hitl_timed_out"
	// EventTypeRegenerated marks the start of an AgentLoop.Regenerate replay.
	// It is the first frame emitted on the regenerate stream — UIs use it to
	// gray out / supersede the previous assistant turn before the replacement
	// content starts arriving. Payload: RegeneratedEvent.
	EventTypeRegenerated StreamEventType = "regenerated"
	// EventTypeContinued marks the start of an AgentLoop.Continue resume.
	// Emitted as the first frame so UIs can attach subsequent content to the
	// existing assistant bubble rather than rendering a new turn. Payload:
	// ContinuedEvent.
	EventTypeContinued StreamEventType = "continued"
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
	// because its per-call MaxTokens cap fired (Anthropic stop_reason
	// "max_tokens"). The truncated text is still in the stream — adopters
	// that need full output should bump the provider's MaxTokens.
	LimitKindProviderMaxTokens LimitKind = "provider_max_tokens"
)

// EventPayload is a sealed interface implemented by every typed event. Use it
// in a type switch to pattern-match a StreamEvent's payload:
//
//	switch p := ev.Payload().(type) {
//	case agent.ContentEvent:
//	    buf.WriteString(p.Text)
//	case agent.UsageEvent:
//	    total += p.Usage.TotalTokens
//	case agent.ErrorEvent:
//	    return p.Err
//	}
//
// The sealing method is unexported, so only this package can add payload
// types — exhaustive matches stay stable across versions.
type EventPayload interface {
	isEventPayload()
}

// BaseEvent carries the correlation fields shared by every event. It is
// embedded into each payload type so consumers reach Source and ParentID
// uniformly regardless of which concrete type they matched.
type BaseEvent struct {
	Source   string
	ParentID string
}

// ContentEvent is assistant-produced text shown to the user.
type ContentEvent struct {
	BaseEvent
	Text string
}

// ThoughtEvent is internal reasoning / system narration. Suppressed from the
// final answer by RunIteration; surfaced by RunIterationStream when
// EmitThoughts is true.
type ThoughtEvent struct {
	BaseEvent
	Message string
}

// ToolCallEvent announces that the agent is about to execute a tool.
// Name is the bare tool identifier (e.g. "web_search"). ID is the agent-
// generated correlation ID — it matches the toolCallID parameter on
// ToolResultHook so observability tooling can pair entry and exit events
// reliably even when SpeculativeTools=true interleaves parallel calls.
// ArgsJSON is the raw tool arguments. Reused is true when the wave executor
// is consuming a speculative result instead of dispatching the tool again.
// Description is a human-readable summary kept for log readers —
// programmatic consumers should use the structured fields.
type ToolCallEvent struct {
	BaseEvent
	ID          string
	Name        string
	ArgsJSON    string
	Reused      bool
	Description string
}

// ToolProgressEvent is a mid-execution status update emitted by a tool via
// tools.ReportProgress. Progress is lossy by design — consumers may drop
// these safely. Name and ToolCallID match the preceding ToolCallEvent for
// the same dispatch; with SpeculativeTools=true multiple tools can report
// progress concurrently, so adopters need both to attribute the message.
type ToolProgressEvent struct {
	BaseEvent
	Name       string
	ToolCallID string
	Message    string
}

// ActionRequiredEvent signals that a tool invocation needs human approval.
// Tool and Args are extracted from the default payload; callers that emit a
// richer envelope (e.g. an approval_id) can fall back to RawJSON.
type ActionRequiredEvent struct {
	BaseEvent
	Tool    string
	Args    string
	RawJSON string
}

// UsageEvent reports token accounting from the most recent LLM call. Usage
// holds the parsed struct; RawJSON is kept for callers that want the exact
// bytes emitted on the wire.
type UsageEvent struct {
	BaseEvent
	Usage   TokenUsage
	RawJSON string
}

// ErrorEvent signals a terminal failure for the current iteration. Err holds
// the structured error (usable with errors.Is / errors.As); Message is its
// string form for callers that only need display text.
type ErrorEvent struct {
	BaseEvent
	Err     error
	Message string
}

// DoneEvent marks the end of the stream — no more events will arrive.
type DoneEvent struct {
	BaseEvent
}

// ReflectedEvent delivers a post-critique canonical answer. Round indicates
// which self-critique pass produced it (1-indexed); consumers typically keep
// the last seen payload as the authoritative response.
type ReflectedEvent struct {
	BaseEvent
	Text  string
	Round int
}

// ToolCallReadyEvent announces that a tool invocation has been fully parsed
// from the LLM stream and is eligible for execution even though the overall
// response is still streaming. AgentLoop.SpeculativeTools turns this signal
// into actual parallel execution for safe calls.
type ToolCallReadyEvent struct {
	BaseEvent
	ID       string
	Name     string
	ArgsJSON string
}

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
	BaseEvent
	Tasks   []TaskListItem
	RawJSON string
}

// MaxItersReachedEvent signals that the loop exhausted its iteration cap
// without a final answer. Limit echoes AgentLoop.MaxIters at the time of
// the failure so consumers can render "ran 15/15 iterations" without
// reaching back into agent state.
type MaxItersReachedEvent struct {
	BaseEvent
	Limit int
}

// SessionCreatedEvent is the first frame of a stream when the loop is
// running a never-before-seen session. Carries the session key so SSE
// relays can forward the signal to the FE for sidebar pinning, and so
// integrators can hang one-shot work (auto-title, welcome payload,
// budget allocation) off the event without racing on history-emptiness.
type SessionCreatedEvent struct {
	BaseEvent
	SessionKey string
}

// LimitExhaustedEvent is the typed payload of EventTypeLimitExhausted.
// Kind identifies which cap fired (see LimitKind constants); Limit is the
// configured ceiling and Used is the actual count at the moment of the
// trip. For provider truncation kinds, Limit is the provider's per-call
// MaxTokens and Used is unset (the provider does not report token usage
// in the same shape).
type LimitExhaustedEvent struct {
	BaseEvent
	Kind  LimitKind
	Limit int
	Used  int
}

// HITLDeniedEvent is the typed payload of EventTypeHITLDenied. Carries the
// tool the gate was guarding so consumers — typically sub-agent wrappers
// rendering "HITL_BLOCKED" to the outer agent — can route on it without
// parsing the raw JSON Content. RawJSON preserves the wire bytes for
// forward-compatible adopters.
type HITLDeniedEvent struct {
	BaseEvent
	Tool    string
	Args    string
	RawJSON string
}

// HITLTimedOutEvent is the typed payload of EventTypeHITLTimedOut. Mirrors
// HITLDeniedEvent and additionally carries the configured Timeout so a UI
// can show "approval expired after 2m" without reaching back into agent
// state.
type HITLTimedOutEvent struct {
	BaseEvent
	Tool    string
	Args    string
	Timeout time.Duration
	RawJSON string
}

// RegeneratedEvent is the typed payload of EventTypeRegenerated.
// PreviousAssistantIndex is the index of the final assistant message in the
// pre-regenerate history — UIs mark that bubble as superseded. TruncatedAt is
// the length of the rewound prefix (msgs[:TruncatedAt] is what survived the
// safe-boundary truncation). Both indices refer to the history snapshot the
// adopter held at the moment the call ran; downstream events update the
// session beyond that point.
type RegeneratedEvent struct {
	BaseEvent
	PreviousAssistantIndex int
	TruncatedAt            int
}

// ContinuedEvent is the typed payload of EventTypeContinued.
// ContinuedFromIndex is the index of the last persisted message at the moment
// Continue was invoked — adopters can use it to anchor "resumed from here" UI
// (e.g. attaching new content to the existing assistant bubble).
type ContinuedEvent struct {
	BaseEvent
	ContinuedFromIndex int
}

// UnknownEvent wraps an event whose Type was not recognized. It preserves
// the raw wire fields so forward-compatible consumers can still inspect
// events produced by a newer version of the framework.
type UnknownEvent struct {
	BaseEvent
	Type    StreamEventType
	Content string
}

func (ContentEvent) isEventPayload()        {}
func (ThoughtEvent) isEventPayload()        {}
func (ToolCallEvent) isEventPayload()       {}
func (ToolProgressEvent) isEventPayload()   {}
func (ActionRequiredEvent) isEventPayload() {}
func (UsageEvent) isEventPayload()          {}
func (ErrorEvent) isEventPayload()          {}
func (DoneEvent) isEventPayload()           {}
func (ReflectedEvent) isEventPayload()      {}
func (ToolCallReadyEvent) isEventPayload()  {}
func (TaskListEvent) isEventPayload()        {}
func (MaxItersReachedEvent) isEventPayload() {}
func (SessionCreatedEvent) isEventPayload()  {}
func (LimitExhaustedEvent) isEventPayload()  {}
func (HITLDeniedEvent) isEventPayload()      {}
func (HITLTimedOutEvent) isEventPayload()    {}
func (RegeneratedEvent) isEventPayload()     {}
func (ContinuedEvent) isEventPayload()       {}
func (UnknownEvent) isEventPayload()         {}

// Per-type constructors. Each returns the concrete payload struct so callers
// can stay unboxed when they know which payload they want — Visit dispatches
// through these without ever paying the EventPayload interface allocation.

func (ev StreamEvent) base() BaseEvent {
	return BaseEvent{Source: ev.Source, ParentID: ev.ParentID}
}

func (ev StreamEvent) asContent() ContentEvent {
	return ContentEvent{BaseEvent: ev.base(), Text: ev.Content}
}

func (ev StreamEvent) asThought() ThoughtEvent {
	return ThoughtEvent{BaseEvent: ev.base(), Message: ev.Content}
}

func (ev StreamEvent) asToolCall() ToolCallEvent {
	name := ev.Name
	if name == "" {
		name = parseExecutingName(ev.Content)
	}
	return ToolCallEvent{
		BaseEvent:   ev.base(),
		ID:          ev.ToolCallID,
		Name:        name,
		ArgsJSON:    ev.ArgsJSON,
		Reused:      ev.Reused,
		Description: ev.Content,
	}
}

func (ev StreamEvent) asToolProgress() ToolProgressEvent {
	return ToolProgressEvent{
		BaseEvent:  ev.base(),
		Name:       ev.Name,
		ToolCallID: ev.ToolCallID,
		Message:    ev.Content,
	}
}

func (ev StreamEvent) asActionRequired() ActionRequiredEvent {
	out := ActionRequiredEvent{BaseEvent: ev.base(), RawJSON: ev.Content}
	var decoded struct {
		Tool string `json:"tool"`
		Args string `json:"args"`
	}
	if err := json.Unmarshal([]byte(ev.Content), &decoded); err == nil {
		out.Tool = decoded.Tool
		out.Args = decoded.Args
	}
	return out
}

func (ev StreamEvent) asUsage() UsageEvent {
	out := UsageEvent{BaseEvent: ev.base(), RawJSON: ev.Content}
	_ = json.Unmarshal([]byte(ev.Content), &out.Usage) // zero-value on failure
	return out
}

func (ev StreamEvent) asError() ErrorEvent {
	msg := ev.Content
	err := ev.Err
	if err == nil && msg != "" {
		err = fmt.Errorf("agent: %s", msg)
	}
	return ErrorEvent{BaseEvent: ev.base(), Err: err, Message: msg}
}

func (ev StreamEvent) asDone() DoneEvent {
	return DoneEvent{BaseEvent: ev.base()}
}

func (ev StreamEvent) asReflected() ReflectedEvent {
	out := ReflectedEvent{BaseEvent: ev.base()}
	var decoded struct {
		Text  string `json:"text"`
		Round int    `json:"round"`
	}
	if err := json.Unmarshal([]byte(ev.Content), &decoded); err == nil && decoded.Text != "" {
		out.Text = decoded.Text
		out.Round = decoded.Round
	} else {
		// Fallback for callers that emitted the raw text.
		out.Text = ev.Content
	}
	return out
}

func (ev StreamEvent) asToolCallReady() ToolCallReadyEvent {
	out := ToolCallReadyEvent{BaseEvent: ev.base()}
	var decoded struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Args string `json:"args"`
	}
	_ = json.Unmarshal([]byte(ev.Content), &decoded)
	out.ID = decoded.ID
	out.Name = decoded.Name
	out.ArgsJSON = decoded.Args
	return out
}

func (ev StreamEvent) asTaskList() TaskListEvent {
	out := TaskListEvent{BaseEvent: ev.base(), RawJSON: ev.Content}
	_ = json.Unmarshal([]byte(ev.Content), &out.Tasks) // empty slice on failure
	return out
}

func (ev StreamEvent) asMaxItersReached() MaxItersReachedEvent {
	out := MaxItersReachedEvent{BaseEvent: ev.base()}
	var decoded struct {
		Limit int `json:"limit"`
	}
	_ = json.Unmarshal([]byte(ev.Content), &decoded) // zero on failure
	out.Limit = decoded.Limit
	return out
}

func (ev StreamEvent) asSessionCreated() SessionCreatedEvent {
	out := SessionCreatedEvent{BaseEvent: ev.base()}
	var decoded struct {
		SessionKey string `json:"session_key"`
	}
	_ = json.Unmarshal([]byte(ev.Content), &decoded)
	out.SessionKey = decoded.SessionKey
	return out
}

func (ev StreamEvent) asLimitExhausted() LimitExhaustedEvent {
	out := LimitExhaustedEvent{BaseEvent: ev.base()}
	var decoded struct {
		Kind  string `json:"kind"`
		Limit int    `json:"limit"`
		Used  int    `json:"used"`
	}
	_ = json.Unmarshal([]byte(ev.Content), &decoded)
	out.Kind = LimitKind(decoded.Kind)
	out.Limit = decoded.Limit
	out.Used = decoded.Used
	return out
}

func (ev StreamEvent) asHITLDenied() HITLDeniedEvent {
	out := HITLDeniedEvent{BaseEvent: ev.base(), RawJSON: ev.Content}
	var decoded struct {
		Tool string `json:"tool"`
		Args string `json:"args"`
	}
	_ = json.Unmarshal([]byte(ev.Content), &decoded)
	out.Tool = decoded.Tool
	out.Args = decoded.Args
	return out
}

func (ev StreamEvent) asHITLTimedOut() HITLTimedOutEvent {
	out := HITLTimedOutEvent{BaseEvent: ev.base(), RawJSON: ev.Content}
	var decoded struct {
		Tool      string `json:"tool"`
		Args      string `json:"args"`
		TimeoutMs int64  `json:"timeout_ms"`
	}
	_ = json.Unmarshal([]byte(ev.Content), &decoded)
	out.Tool = decoded.Tool
	out.Args = decoded.Args
	out.Timeout = time.Duration(decoded.TimeoutMs) * time.Millisecond
	return out
}

func (ev StreamEvent) asRegenerated() RegeneratedEvent {
	out := RegeneratedEvent{BaseEvent: ev.base()}
	var decoded struct {
		PreviousAssistantIndex int `json:"previous_assistant_index"`
		TruncatedAt            int `json:"truncated_at"`
	}
	_ = json.Unmarshal([]byte(ev.Content), &decoded)
	out.PreviousAssistantIndex = decoded.PreviousAssistantIndex
	out.TruncatedAt = decoded.TruncatedAt
	return out
}

func (ev StreamEvent) asContinued() ContinuedEvent {
	out := ContinuedEvent{BaseEvent: ev.base()}
	var decoded struct {
		ContinuedFromIndex int `json:"continued_from_index"`
	}
	_ = json.Unmarshal([]byte(ev.Content), &decoded)
	out.ContinuedFromIndex = decoded.ContinuedFromIndex
	return out
}

func (ev StreamEvent) asUnknown() UnknownEvent {
	return UnknownEvent{BaseEvent: ev.base(), Type: ev.Type, Content: ev.Content}
}

// Payload returns a typed view of ev. It never returns nil — unknown types
// round-trip through UnknownEvent so callers can log or forward them without
// a nil check.
func (ev StreamEvent) Payload() EventPayload {
	switch ev.Type {
	case EventTypeContent:
		return ev.asContent()
	case EventTypeThought:
		return ev.asThought()
	case EventTypeToolCall:
		return ev.asToolCall()
	case EventTypeToolProgress:
		return ev.asToolProgress()
	case EventTypeActionRequired:
		return ev.asActionRequired()
	case EventTypeUsage:
		return ev.asUsage()
	case EventTypeError:
		return ev.asError()
	case EventTypeDone:
		return ev.asDone()
	case EventTypeReflected:
		return ev.asReflected()
	case EventTypeToolCallReady:
		return ev.asToolCallReady()
	case EventTypeTaskList:
		return ev.asTaskList()
	case EventTypeMaxItersReached:
		return ev.asMaxItersReached()
	case EventTypeSessionCreated:
		return ev.asSessionCreated()
	case EventTypeLimitExhausted:
		return ev.asLimitExhausted()
	case EventTypeHITLDenied:
		return ev.asHITLDenied()
	case EventTypeHITLTimedOut:
		return ev.asHITLTimedOut()
	case EventTypeRegenerated:
		return ev.asRegenerated()
	case EventTypeContinued:
		return ev.asContinued()
	default:
		return ev.asUnknown()
	}
}

// EventVisitor is a typed double-dispatch interface. Implementing it gives
// the compiler teeth: adding a new payload type forces every visitor in the
// codebase to handle it.
//
// Prefer the type-switch on Payload() for ad-hoc consumers; prefer a visitor
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
	VisitUnknown(UnknownEvent)
}

// Visit dispatches ev to the correct method on v. Switches on ev.Type and
// invokes the visitor with the concrete payload struct — no EventPayload
// interface allocation and no second type switch on a boxed value.
func (ev StreamEvent) Visit(v EventVisitor) {
	switch ev.Type {
	case EventTypeContent:
		v.VisitContent(ev.asContent())
	case EventTypeThought:
		v.VisitThought(ev.asThought())
	case EventTypeToolCall:
		v.VisitToolCall(ev.asToolCall())
	case EventTypeToolProgress:
		v.VisitToolProgress(ev.asToolProgress())
	case EventTypeActionRequired:
		v.VisitActionRequired(ev.asActionRequired())
	case EventTypeUsage:
		v.VisitUsage(ev.asUsage())
	case EventTypeError:
		v.VisitError(ev.asError())
	case EventTypeDone:
		v.VisitDone(ev.asDone())
	case EventTypeReflected:
		v.VisitReflected(ev.asReflected())
	case EventTypeToolCallReady:
		v.VisitToolCallReady(ev.asToolCallReady())
	case EventTypeTaskList:
		v.VisitTaskList(ev.asTaskList())
	case EventTypeMaxItersReached:
		v.VisitMaxItersReached(ev.asMaxItersReached())
	case EventTypeSessionCreated:
		v.VisitSessionCreated(ev.asSessionCreated())
	case EventTypeLimitExhausted:
		v.VisitLimitExhausted(ev.asLimitExhausted())
	case EventTypeHITLDenied:
		v.VisitHITLDenied(ev.asHITLDenied())
	case EventTypeHITLTimedOut:
		v.VisitHITLTimedOut(ev.asHITLTimedOut())
	case EventTypeRegenerated:
		v.VisitRegenerated(ev.asRegenerated())
	case EventTypeContinued:
		v.VisitContinued(ev.asContinued())
	default:
		v.VisitUnknown(ev.asUnknown())
	}
}
