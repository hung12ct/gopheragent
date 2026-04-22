package agent

import (
	"encoding/json"
	"fmt"
)

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
// Description is a human-readable summary (e.g. "Executing: web_search").
type ToolCallEvent struct {
	BaseEvent
	Description string
}

// ToolProgressEvent is a mid-execution status update emitted by a tool via
// tools.ReportProgress. Progress is lossy by design — consumers may drop
// these safely.
type ToolProgressEvent struct {
	BaseEvent
	Message string
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
func (UnknownEvent) isEventPayload()        {}

// Payload returns a typed view of ev. It never returns nil — unknown types
// round-trip through UnknownEvent so callers can log or forward them without
// a nil check.
func (ev StreamEvent) Payload() EventPayload {
	base := BaseEvent{Source: ev.Source, ParentID: ev.ParentID}

	switch ev.Type {
	case EventTypeContent:
		return ContentEvent{BaseEvent: base, Text: ev.Content}
	case EventTypeThought:
		return ThoughtEvent{BaseEvent: base, Message: ev.Content}
	case EventTypeToolCall:
		return ToolCallEvent{BaseEvent: base, Description: ev.Content}
	case EventTypeToolProgress:
		return ToolProgressEvent{BaseEvent: base, Message: ev.Content}
	case EventTypeActionRequired:
		out := ActionRequiredEvent{BaseEvent: base, RawJSON: ev.Content}
		var decoded struct {
			Tool string `json:"tool"`
			Args string `json:"args"`
		}
		if err := json.Unmarshal([]byte(ev.Content), &decoded); err == nil {
			out.Tool = decoded.Tool
			out.Args = decoded.Args
		}
		return out
	case EventTypeUsage:
		out := UsageEvent{BaseEvent: base, RawJSON: ev.Content}
		_ = json.Unmarshal([]byte(ev.Content), &out.Usage) // zero-value on failure
		return out
	case EventTypeError:
		msg := ev.Content
		err := ev.Err
		if err == nil && msg != "" {
			err = fmt.Errorf("agent: %s", msg)
		}
		return ErrorEvent{BaseEvent: base, Err: err, Message: msg}
	case EventTypeDone:
		return DoneEvent{BaseEvent: base}
	case EventTypeReflected:
		out := ReflectedEvent{BaseEvent: base}
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
	case EventTypeToolCallReady:
		out := ToolCallReadyEvent{BaseEvent: base}
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
	default:
		return UnknownEvent{BaseEvent: base, Type: ev.Type, Content: ev.Content}
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
	VisitUnknown(UnknownEvent)
}

// Visit dispatches ev to the correct method on v. It is a thin wrapper over
// Payload() — the two APIs are interchangeable; pick whichever reads clearer
// at the call site.
func (ev StreamEvent) Visit(v EventVisitor) {
	switch p := ev.Payload().(type) {
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
	case UnknownEvent:
		v.VisitUnknown(p)
	}
}
