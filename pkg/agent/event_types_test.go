package agent

import (
	"errors"
	"testing"
)

func TestPayload_EachTypeProducesCorrectConcreteType(t *testing.T) {
	boom := errors.New("kaput")
	cases := []struct {
		ev   StreamEvent
		want string // name of expected concrete type
	}{
		{StreamEvent{Type: EventTypeContent, Content: "hi"}, "ContentEvent"},
		{StreamEvent{Type: EventTypeThought, Content: "hmm"}, "ThoughtEvent"},
		{StreamEvent{Type: EventTypeToolCall, Content: "Executing: x"}, "ToolCallEvent"},
		{StreamEvent{Type: EventTypeToolProgress, Content: "50% done"}, "ToolProgressEvent"},
		{StreamEvent{Type: EventTypeActionRequired, Content: `{"tool":"rm","args":"/"}`}, "ActionRequiredEvent"},
		{StreamEvent{Type: EventTypeUsage, Content: `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`}, "UsageEvent"},
		{StreamEvent{Type: EventTypeError, Content: "bad", Err: boom}, "ErrorEvent"},
		{StreamEvent{Type: EventTypeDone}, "DoneEvent"},
		{StreamEvent{Type: "future_event", Content: "x"}, "UnknownEvent"},
	}
	for _, tc := range cases {
		p := tc.ev.Payload()
		got := typeName(p)
		if got != tc.want {
			t.Errorf("Type=%q: expected %s, got %s", tc.ev.Type, tc.want, got)
		}
	}
}

func TestPayload_ToolCallSurfacesNameDirectly(t *testing.T) {
	// New emitters populate StreamEvent.Name; consumers should never have
	// to parse the "Executing: ..." Content shape.
	ev := StreamEvent{Type: EventTypeToolCall, Name: "web_search", Content: "Executing: web_search"}
	tc, ok := ev.Payload().(ToolCallEvent)
	if !ok {
		t.Fatalf("expected ToolCallEvent, got %T", ev.Payload())
	}
	if tc.Name != "web_search" {
		t.Fatalf("Name: got %q, want %q", tc.Name, "web_search")
	}
	if tc.Description != "Executing: web_search" {
		t.Fatalf("Description: got %q, want %q", tc.Description, "Executing: web_search")
	}
}

// TestPayload_ToolProgressCarriesIDAndName pins the correlation contract
// for mid-execution progress events: under SpeculativeTools=true multiple
// tools can be reporting progress concurrently, so each progress event
// must echo the call ID + tool name from its originating ToolCallEvent.
func TestPayload_ToolProgressCarriesIDAndName(t *testing.T) {
	ev := StreamEvent{
		Type:       EventTypeToolProgress,
		Name:       "web_search",
		ToolCallID: "tcid-77",
		Content:    "fetched 3/5",
	}
	tp, ok := ev.Payload().(ToolProgressEvent)
	if !ok {
		t.Fatalf("expected ToolProgressEvent, got %T", ev.Payload())
	}
	if tp.Name != "web_search" || tp.ToolCallID != "tcid-77" || tp.Message != "fetched 3/5" {
		t.Fatalf("progress event did not round-trip: %+v", tp)
	}
}

// TestPayload_ToolCallCarriesIDAndArgs pins the v0.19.0 wire additions:
// the typed payload exposes ID, ArgsJSON, and Reused so adopters can pair
// entry events to OnToolResult invocations and attribute speculation
// savings without parsing Content.
func TestPayload_ToolCallCarriesIDAndArgs(t *testing.T) {
	ev := StreamEvent{
		Type:       EventTypeToolCall,
		Name:       "web_search",
		ToolCallID: "abc123",
		ArgsJSON:   `{"q":"hi"}`,
		Reused:     true,
		Content:    "Executing: web_search",
	}
	tc, ok := ev.Payload().(ToolCallEvent)
	if !ok {
		t.Fatalf("expected ToolCallEvent, got %T", ev.Payload())
	}
	if tc.ID != "abc123" || tc.ArgsJSON != `{"q":"hi"}` || !tc.Reused {
		t.Fatalf("ID/ArgsJSON/Reused did not round-trip: %+v", tc)
	}
}

func TestPayload_ToolCallFallsBackToContentParse(t *testing.T) {
	// Back-compat: events serialized by older versions only carry the
	// magic-string Content. Payload() must still recover Name.
	ev := StreamEvent{Type: EventTypeToolCall, Content: "Executing: web_search"}
	tc := ev.Payload().(ToolCallEvent)
	if tc.Name != "web_search" {
		t.Fatalf("legacy Content fallback: got %q, want %q", tc.Name, "web_search")
	}
}

func TestPayload_MaxItersReachedDecodesLimit(t *testing.T) {
	ev := StreamEvent{Type: EventTypeMaxItersReached, Content: `{"limit":15}`}
	p, ok := ev.Payload().(MaxItersReachedEvent)
	if !ok {
		t.Fatalf("expected MaxItersReachedEvent, got %T", ev.Payload())
	}
	if p.Limit != 15 {
		t.Fatalf("Limit: got %d, want 15", p.Limit)
	}
}

func TestPayload_NeverReturnsNil(t *testing.T) {
	// Empty Type must also map to UnknownEvent rather than nil.
	if p := (StreamEvent{}).Payload(); p == nil {
		t.Fatal("Payload() must never return nil")
	}
}

func TestPayload_UsageParsesJSON(t *testing.T) {
	ev := StreamEvent{Type: EventTypeUsage, Content: `{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}`}
	u, ok := ev.Payload().(UsageEvent)
	if !ok {
		t.Fatalf("expected UsageEvent, got %T", ev.Payload())
	}
	if u.Usage.PromptTokens != 100 || u.Usage.CompletionTokens != 50 || u.Usage.TotalTokens != 150 {
		t.Fatalf("usage parsing failed: %+v", u.Usage)
	}
	if u.RawJSON == "" {
		t.Fatal("RawJSON should be preserved for fallback access")
	}
}

func TestPayload_UsageMalformedJSONYieldsZeroValueNotPanic(t *testing.T) {
	ev := StreamEvent{Type: EventTypeUsage, Content: "not json"}
	u, ok := ev.Payload().(UsageEvent)
	if !ok {
		t.Fatalf("expected UsageEvent even for malformed content, got %T", ev.Payload())
	}
	if u.Usage.TotalTokens != 0 {
		t.Fatalf("expected zero TokenUsage on parse failure, got %+v", u.Usage)
	}
	if u.RawJSON != "not json" {
		t.Fatalf("RawJSON should preserve the original content, got %q", u.RawJSON)
	}
}

func TestPayload_ActionRequiredExtractsToolAndArgs(t *testing.T) {
	ev := StreamEvent{Type: EventTypeActionRequired, Content: `{"tool":"shell_exec","args":"rm -rf /","approval_id":"abc"}`}
	a, ok := ev.Payload().(ActionRequiredEvent)
	if !ok {
		t.Fatalf("expected ActionRequiredEvent, got %T", ev.Payload())
	}
	if a.Tool != "shell_exec" || a.Args != "rm -rf /" {
		t.Fatalf("extraction failed: %+v", a)
	}
	if a.RawJSON == "" {
		t.Fatal("RawJSON should be preserved so callers can read custom fields (e.g. approval_id)")
	}
}

func TestPayload_ErrorPrefersStructuredErr(t *testing.T) {
	boom := errors.New("disk on fire")
	ev := StreamEvent{Type: EventTypeError, Content: "disk on fire", Err: boom}
	e, ok := ev.Payload().(ErrorEvent)
	if !ok {
		t.Fatalf("expected ErrorEvent, got %T", ev.Payload())
	}
	if !errors.Is(e.Err, boom) {
		t.Fatalf("structured Err not preserved: %v", e.Err)
	}
	if e.Message != "disk on fire" {
		t.Fatalf("Message not preserved: %q", e.Message)
	}
}

func TestPayload_ErrorSynthesizesErrWhenMissing(t *testing.T) {
	// Producers occasionally emit Content without attaching Err. Payload must
	// still return an ErrorEvent whose Err is non-nil so downstream switches
	// on `case ErrorEvent` don't need to juggle a bare string.
	ev := StreamEvent{Type: EventTypeError, Content: "rate limited"}
	e := ev.Payload().(ErrorEvent)
	if e.Err == nil {
		t.Fatal("Err should be synthesized from Message when missing")
	}
	if e.Err.Error() == "" {
		t.Fatal("synthesized Err must carry the original message")
	}
}

func TestPayload_CorrelationFieldsPropagate(t *testing.T) {
	ev := StreamEvent{
		Type:     EventTypeThought,
		Content:  "inner",
		Source:   "subagent:A>subagent:B",
		ParentID: "session-root",
	}
	p := ev.Payload().(ThoughtEvent)
	if p.Source != "subagent:A>subagent:B" || p.ParentID != "session-root" {
		t.Fatalf("correlation fields not propagated: %+v", p)
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
func (r *recordingVisitor) VisitTaskList(TaskListEvent)               { r.visited = "task_list" }
func (r *recordingVisitor) VisitMaxItersReached(MaxItersReachedEvent)  { r.visited = "max_iters_reached" }
func (r *recordingVisitor) VisitSessionCreated(SessionCreatedEvent)    { r.visited = "session_created" }
func (r *recordingVisitor) VisitLimitExhausted(LimitExhaustedEvent)    { r.visited = "limit_exhausted" }
func (r *recordingVisitor) VisitHITLDenied(HITLDeniedEvent)            { r.visited = "hitl_denied" }
func (r *recordingVisitor) VisitHITLTimedOut(HITLTimedOutEvent)        { r.visited = "hitl_timed_out" }
func (r *recordingVisitor) VisitRegenerated(RegeneratedEvent)         { r.visited = "regenerated" }
func (r *recordingVisitor) VisitContinued(ContinuedEvent)             { r.visited = "continued" }
func (r *recordingVisitor) VisitUnknown(UnknownEvent)                 { r.visited = "unknown" }

func TestVisit_DispatchesToMatchingMethod(t *testing.T) {
	cases := []struct {
		ev   StreamEvent
		want string
	}{
		{StreamEvent{Type: EventTypeContent}, "content"},
		{StreamEvent{Type: EventTypeThought}, "thought"},
		{StreamEvent{Type: EventTypeToolCall}, "tool_call"},
		{StreamEvent{Type: EventTypeToolProgress}, "tool_progress"},
		{StreamEvent{Type: EventTypeActionRequired, Content: "{}"}, "action_required"},
		{StreamEvent{Type: EventTypeUsage, Content: "{}"}, "usage"},
		{StreamEvent{Type: EventTypeError, Content: "x"}, "error"},
		{StreamEvent{Type: EventTypeDone}, "done"},
		{StreamEvent{Type: EventTypeReflected, Content: `{"text":"ok","round":1}`}, "reflected"},
		{StreamEvent{Type: EventTypeToolCallReady, Content: `{"id":"c1","name":"x","args":"{}"}`}, "tool_call_ready"},
		{StreamEvent{Type: EventTypeTaskList, Content: `[]`}, "task_list"},
		{StreamEvent{Type: EventTypeMaxItersReached, Content: `{"limit":15}`}, "max_iters_reached"},
		{StreamEvent{Type: EventTypeSessionCreated, Content: `{"session_key":"alice:abc"}`}, "session_created"},
		{StreamEvent{Type: EventTypeLimitExhausted, Content: `{"kind":"max_iters","limit":15,"used":15}`}, "limit_exhausted"},
		{StreamEvent{Type: EventTypeHITLDenied, Content: `{"tool":"x","args":"{}"}`}, "hitl_denied"},
		{StreamEvent{Type: EventTypeHITLTimedOut, Content: `{"tool":"x","args":"{}","timeout_ms":1000}`}, "hitl_timed_out"},
		{StreamEvent{Type: EventTypeRegenerated, Content: `{"previous_assistant_index":3,"truncated_at":2}`}, "regenerated"},
		{StreamEvent{Type: EventTypeContinued, Content: `{"continued_from_index":7}`}, "continued"},
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

// typeName is a small reflection-free helper keeping the table-test readable.
func typeName(p EventPayload) string {
	switch p.(type) {
	case ContentEvent:
		return "ContentEvent"
	case ThoughtEvent:
		return "ThoughtEvent"
	case ToolCallEvent:
		return "ToolCallEvent"
	case ToolProgressEvent:
		return "ToolProgressEvent"
	case ActionRequiredEvent:
		return "ActionRequiredEvent"
	case UsageEvent:
		return "UsageEvent"
	case ErrorEvent:
		return "ErrorEvent"
	case DoneEvent:
		return "DoneEvent"
	case UnknownEvent:
		return "UnknownEvent"
	default:
		return "<nil-or-unrecognised>"
	}
}
