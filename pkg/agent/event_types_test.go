package agent

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestEvent_ConstructorMatchesType(t *testing.T) {
	// Spot-check Event() derives Type from the payload's static type.
	cases := []struct {
		ev   StreamEvent
		want StreamEventType
	}{
		{Event(ContentEvent{Text: "hi"}), EventTypeContent},
		{Event(ThoughtEvent{Message: "hmm"}), EventTypeThought},
		{Event(ToolCallEvent{ID: "c1", Name: "ws"}), EventTypeToolCall},
		{Event(UsageEvent{}), EventTypeUsage},
		{Event(DoneEvent{}), EventTypeDone},
	}
	for _, tc := range cases {
		if tc.ev.Type != tc.want {
			t.Errorf("Event(%T) produced Type=%q, want %q", tc.ev.Payload, tc.ev.Type, tc.want)
		}
		if tc.ev.Payload == nil {
			t.Errorf("Event(%T) left Payload nil", tc.ev.Payload)
		}
	}
}

func TestEvent_TypedFieldsRoundTrip(t *testing.T) {
	ev := Event(ToolCallEvent{ID: "abc123", Name: "web_search", ArgsJSON: `{"q":"hi"}`, Reused: true})
	tc, ok := ev.Payload.(ToolCallEvent)
	if !ok {
		t.Fatalf("expected ToolCallEvent, got %T", ev.Payload)
	}
	if tc.ID != "abc123" || tc.Name != "web_search" || tc.ArgsJSON != `{"q":"hi"}` || !tc.Reused {
		t.Fatalf("typed fields did not round-trip: %+v", tc)
	}
}

func TestEvent_CorrelationFieldsLiveOnEnvelope(t *testing.T) {
	// Source/ParentID live on StreamEvent, not on the payload — sub-agent
	// forwarders mutate them after construction.
	ev := Event(ThoughtEvent{Message: "inner"})
	ev.Source = "subagent:A>subagent:B"
	ev.ParentID = "session-root"
	if ev.Source != "subagent:A>subagent:B" || ev.ParentID != "session-root" {
		t.Fatalf("correlation fields lost: %+v", ev)
	}
	// The payload itself does not carry them — keeping the model lean.
	p := ev.Payload.(ThoughtEvent)
	if p.Message != "inner" {
		t.Fatalf("payload corrupted by envelope mutation: %+v", p)
	}
}

// --- wire format (JSON) ---

func TestStreamEvent_RoundTripsThroughJSON(t *testing.T) {
	cases := []StreamEvent{
		Event(ContentEvent{Text: "hi"}),
		Event(ThoughtEvent{Message: "hmm"}),
		Event(ToolCallEvent{ID: "c1", Name: "ws", ArgsJSON: `{"q":"hi"}`, Reused: true}),
		Event(ToolProgressEvent{Name: "ws", ToolCallID: "c1", Message: "50% done"}),
		Event(ActionRequiredEvent{Tool: "rm", Args: "/"}),
		Event(UsageEvent{Usage: TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}}),
		Event(DoneEvent{}),
		Event(ReflectedEvent{Text: "ok", Round: 1}),
		Event(ToolCallReadyEvent{ID: "c1", Name: "x", ArgsJSON: `{}`}),
		Event(MaxItersReachedEvent{Limit: 15}),
		Event(SessionCreatedEvent{SessionKey: "alice:abc"}),
		Event(LimitExhaustedEvent{Kind: LimitKindMaxIters, Limit: 15, Used: 15}),
		Event(HITLDeniedEvent{Tool: "x", Args: "{}"}),
		Event(HITLTimedOutEvent{Tool: "x", Args: "{}", Timeout: time.Second}),
		Event(RegeneratedEvent{PreviousAssistantIndex: 3, TruncatedAt: 2}),
		Event(ContinuedEvent{ContinuedFromIndex: 7}),
		Event(TaskListEvent{Tasks: []TaskListItem{{ID: "t1", Title: "one", Status: "pending"}}}),
	}
	for _, ev := range cases {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal %T: %v", ev.Payload, err)
		}
		var got StreamEvent
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal %T: %v\nwire: %s", ev.Payload, err, data)
		}
		if got.Type != ev.Type {
			t.Errorf("type round-trip lost: %T marshaled to %s but unmarshaled to %s", ev.Payload, ev.Type, got.Type)
		}
		// Sanity-check that the payload concrete type matches.
		if got.Payload == nil {
			t.Errorf("%T: unmarshaled Payload is nil", ev.Payload)
		}
	}
}

func TestStreamEvent_UnknownTypeRoundTripsThroughUnknownEvent(t *testing.T) {
	wire := []byte(`{"type":"future_event","payload":{"foo":42}}`)
	var got StreamEvent
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "future_event" {
		t.Errorf("expected type future_event, got %q", got.Type)
	}
	u, ok := got.Payload.(UnknownEvent)
	if !ok {
		t.Fatalf("expected UnknownEvent for novel type, got %T", got.Payload)
	}
	if u.OriginalType != "future_event" {
		t.Errorf("OriginalType: got %q, want future_event", u.OriginalType)
	}
}

func TestStreamEvent_ErrorEventCarriesErrButErrIsNotMarshaled(t *testing.T) {
	boom := errors.New("disk on fire")
	ev := Event(ErrorEvent{Err: boom, Message: "disk on fire"})
	data, _ := json.Marshal(ev)
	// Err has json:"-" so the wire shape does not include it.
	if string(data) == "" {
		t.Fatal("marshaled to empty")
	}
	var got StreamEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p, ok := got.Payload.(ErrorEvent)
	if !ok {
		t.Fatalf("expected ErrorEvent, got %T", got.Payload)
	}
	// The unmarshal path synthesizes Err from Message when the wire was
	// produced by a marshaler that dropped Err — exactly the SSE adopter case.
	if p.Err == nil {
		t.Fatal("Err should be synthesized from Message on the unmarshal path")
	}
	if p.Message != "disk on fire" {
		t.Fatalf("Message lost: %q", p.Message)
	}
}

// --- visitor ---

type recordingVisitor struct {
	visited string
}

func (r *recordingVisitor) VisitContent(ContentEvent)               { r.visited = "content" }
func (r *recordingVisitor) VisitThought(ThoughtEvent)               { r.visited = "thought" }
func (r *recordingVisitor) VisitToolCall(ToolCallEvent)             { r.visited = "tool_call" }
func (r *recordingVisitor) VisitToolProgress(ToolProgressEvent)     { r.visited = "tool_progress" }
func (r *recordingVisitor) VisitActionRequired(ActionRequiredEvent) { r.visited = "action_required" }
func (r *recordingVisitor) VisitUsage(UsageEvent)                   { r.visited = "usage" }
func (r *recordingVisitor) VisitError(ErrorEvent)                   { r.visited = "error" }
func (r *recordingVisitor) VisitDone(DoneEvent)                     { r.visited = "done" }
func (r *recordingVisitor) VisitReflected(ReflectedEvent)           { r.visited = "reflected" }
func (r *recordingVisitor) VisitToolCallReady(ToolCallReadyEvent)   { r.visited = "tool_call_ready" }
func (r *recordingVisitor) VisitTaskList(TaskListEvent)             { r.visited = "task_list" }
func (r *recordingVisitor) VisitMaxItersReached(MaxItersReachedEvent) {
	r.visited = "max_iters_reached"
}
func (r *recordingVisitor) VisitSessionCreated(SessionCreatedEvent) { r.visited = "session_created" }
func (r *recordingVisitor) VisitLimitExhausted(LimitExhaustedEvent) { r.visited = "limit_exhausted" }
func (r *recordingVisitor) VisitHITLDenied(HITLDeniedEvent)         { r.visited = "hitl_denied" }
func (r *recordingVisitor) VisitHITLTimedOut(HITLTimedOutEvent)     { r.visited = "hitl_timed_out" }
func (r *recordingVisitor) VisitRegenerated(RegeneratedEvent)       { r.visited = "regenerated" }
func (r *recordingVisitor) VisitContinued(ContinuedEvent)           { r.visited = "continued" }
func (r *recordingVisitor) VisitMemoryLoaded(MemoryLoadedEvent)     { r.visited = "memory_loaded" }
func (r *recordingVisitor) VisitMemoryConsolidated(MemoryConsolidatedEvent) {
	r.visited = "memory_consolidated"
}
func (r *recordingVisitor) VisitRunCost(RunCostEvent)           { r.visited = "run_cost" }
func (r *recordingVisitor) VisitContextTrace(ContextTraceEvent) { r.visited = "context_trace" }
func (r *recordingVisitor) VisitDegraded(DegradedEvent)         { r.visited = "degraded" }
func (r *recordingVisitor) VisitUnknown(UnknownEvent)           { r.visited = "unknown" }

func TestVisit_DispatchesToMatchingMethod(t *testing.T) {
	cases := []struct {
		ev   StreamEvent
		want string
	}{
		{Event(ContentEvent{}), "content"},
		{Event(ThoughtEvent{}), "thought"},
		{Event(ToolCallEvent{}), "tool_call"},
		{Event(ToolProgressEvent{}), "tool_progress"},
		{Event(ActionRequiredEvent{}), "action_required"},
		{Event(UsageEvent{}), "usage"},
		{Event(ErrorEvent{}), "error"},
		{Event(DoneEvent{}), "done"},
		{Event(ReflectedEvent{}), "reflected"},
		{Event(ToolCallReadyEvent{}), "tool_call_ready"},
		{Event(TaskListEvent{}), "task_list"},
		{Event(MaxItersReachedEvent{}), "max_iters_reached"},
		{Event(SessionCreatedEvent{}), "session_created"},
		{Event(LimitExhaustedEvent{}), "limit_exhausted"},
		{Event(HITLDeniedEvent{}), "hitl_denied"},
		{Event(HITLTimedOutEvent{}), "hitl_timed_out"},
		{Event(RegeneratedEvent{}), "regenerated"},
		{Event(ContinuedEvent{}), "continued"},
		{StreamEvent{Type: "novel_type"}, "unknown"},
	}
	for _, tc := range cases {
		v := &recordingVisitor{}
		tc.ev.Visit(v)
		if v.visited != tc.want {
			t.Errorf("Type=%q: visitor routed to %q, expected %q", tc.ev.Type, v.visited, tc.want)
		}
	}
}
