package eval

import (
	"context"
	"iter"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
)

// stubTarget yields a fixed sequence of events, ignoring its inputs. It
// optionally serves history for the HistoryReader join.
type stubTarget struct {
	events  []agent.StreamEvent
	history []history.Message
}

func (s stubTarget) Run(_ context.Context, _ string, _ history.Message) iter.Seq[agent.StreamEvent] {
	return func(yield func(agent.StreamEvent) bool) {
		for _, ev := range s.events {
			if !yield(ev) {
				return
			}
		}
	}
}

func (s stubTarget) History(_ context.Context, _ string) ([]history.Message, error) {
	return s.history, nil
}

// stubNoHistory is a Target that does NOT implement HistoryReader.
type stubNoHistory struct{ events []agent.StreamEvent }

func (s stubNoHistory) Run(_ context.Context, _ string, _ history.Message) iter.Seq[agent.StreamEvent] {
	return stubTarget{events: s.events}.Run(context.Background(), "", history.Message{})
}

func sub(ev agent.StreamEvent, source string) agent.StreamEvent {
	ev.Source = source
	return ev
}

func TestCaptureAccumulatesContentAndTools(t *testing.T) {
	target := stubTarget{
		events: []agent.StreamEvent{
			agent.Event(agent.ToolCallEvent{ID: "c1", Name: "get_weather", ArgsJSON: `{"city":"Tokyo"}`}),
			agent.Event(agent.ContentEvent{Text: "It is "}),
			agent.Event(agent.ContentEvent{Text: "21°."}),
			agent.Event(agent.UsageEvent{Usage: agent.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}),
			agent.Event(agent.DoneEvent{}),
		},
		history: []history.Message{
			{Role: "tool", CorrelationID: "c1", Content: `{"temp_c":21}`},
		},
	}
	tr := Capture(context.Background(), target, "k", history.Message{Role: "user", Content: "weather?"}, CaptureOptions{})
	if tr.FinalAnswer != "It is 21°." {
		t.Fatalf("answer = %q", tr.FinalAnswer)
	}
	if len(tr.ToolCalls) != 1 || tr.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("tool calls = %+v", tr.ToolCalls)
	}
	if tr.ToolCalls[0].Result != `{"temp_c":21}` {
		t.Fatalf("tool result not joined: %q", tr.ToolCalls[0].Result)
	}
	if tr.Usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v", tr.Usage)
	}
	if tr.TerminatedBy != "done" {
		t.Fatalf("terminated = %q", tr.TerminatedBy)
	}
}

func TestCaptureReflectedReplacesAnswer(t *testing.T) {
	target := stubTarget{events: []agent.StreamEvent{
		agent.Event(agent.ContentEvent{Text: "draft answer"}),
		agent.Event(agent.ReflectedEvent{Text: "final answer", Round: 1}),
		agent.Event(agent.DoneEvent{}),
	}}
	tr := Capture(context.Background(), target, "k", history.Message{}, CaptureOptions{})
	if tr.FinalAnswer != "final answer" {
		t.Fatalf("reflected did not replace: %q", tr.FinalAnswer)
	}
}

func TestCaptureFiltersSubagentEvents(t *testing.T) {
	events := []agent.StreamEvent{
		sub(agent.Event(agent.ToolCallEvent{ID: "s1", Name: "sub_tool"}), "subagent:worker"),
		sub(agent.Event(agent.ContentEvent{Text: "sub text"}), "subagent:worker"),
		agent.Event(agent.ToolCallEvent{ID: "c1", Name: "top_tool"}),
		agent.Event(agent.ContentEvent{Text: "top text"}),
		agent.Event(agent.DoneEvent{}),
	}
	// Default: sub-agent events excluded.
	tr := Capture(context.Background(), stubTarget{events: events}, "k", history.Message{}, CaptureOptions{})
	if tr.FinalAnswer != "top text" {
		t.Fatalf("answer leaked sub text: %q", tr.FinalAnswer)
	}
	if len(tr.ToolCalls) != 1 || tr.ToolCalls[0].Name != "top_tool" {
		t.Fatalf("sub tool leaked: %+v", tr.ToolCalls)
	}
	// Opt-in: sub-agent tool calls included, answer still top-level only.
	tr = Capture(context.Background(), stubTarget{events: events}, "k", history.Message{}, CaptureOptions{IncludeSubagents: true})
	if len(tr.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls with IncludeSubagents, got %+v", tr.ToolCalls)
	}
	if tr.FinalAnswer != "top text" {
		t.Fatalf("answer should stay top-level: %q", tr.FinalAnswer)
	}
}

func TestCaptureTerminationReasons(t *testing.T) {
	cases := []struct {
		name string
		ev   agent.EventPayload
		want string
	}{
		{"error", agent.ErrorEvent{Message: "boom"}, "error"},
		{"max_iters", agent.MaxItersReachedEvent{Limit: 5}, "max_iters"},
		{"limit", agent.LimitExhaustedEvent{Kind: agent.LimitKindMaxToolCallsPerSession}, "limit_exhausted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := Capture(context.Background(), stubTarget{events: []agent.StreamEvent{agent.Event(tc.ev)}}, "k", history.Message{}, CaptureOptions{})
			if tr.TerminatedBy != tc.want {
				t.Fatalf("terminated = %q want %q", tr.TerminatedBy, tc.want)
			}
		})
	}
}

func TestCaptureMaxItersSurvivesTrailingError(t *testing.T) {
	// The loop emits MaxItersReachedEvent then a legacy ErrorEvent; the more
	// specific max_iters label must win.
	tr := Capture(context.Background(), stubTarget{events: []agent.StreamEvent{
		agent.Event(agent.MaxItersReachedEvent{Limit: 3}),
		agent.Event(agent.ErrorEvent{Message: "max iterations"}),
	}}, "k", history.Message{}, CaptureOptions{})
	if tr.TerminatedBy != "max_iters" {
		t.Fatalf("terminated = %q, want max_iters", tr.TerminatedBy)
	}
	if tr.Err == nil {
		t.Fatalf("expected Err to be recorded")
	}
}

func TestCaptureCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr := Capture(ctx, stubTarget{events: nil}, "k", history.Message{}, CaptureOptions{})
	if tr.TerminatedBy != "ctx" {
		t.Fatalf("terminated = %q, want ctx", tr.TerminatedBy)
	}
}

func TestCaptureNoHistoryReaderLeavesResultEmpty(t *testing.T) {
	tr := Capture(context.Background(), stubNoHistory{events: []agent.StreamEvent{
		agent.Event(agent.ToolCallEvent{ID: "c1", Name: "t"}),
		agent.Event(agent.DoneEvent{}),
	}}, "k", history.Message{}, CaptureOptions{})
	if len(tr.ToolCalls) != 1 || tr.ToolCalls[0].Result != "" {
		t.Fatalf("expected empty result without HistoryReader: %+v", tr.ToolCalls)
	}
}

func TestCaptureKeepEvents(t *testing.T) {
	events := []agent.StreamEvent{agent.Event(agent.ContentEvent{Text: "hi"}), agent.Event(agent.DoneEvent{})}
	tr := Capture(context.Background(), stubTarget{events: events}, "k", history.Message{}, CaptureOptions{KeepEvents: true})
	if len(tr.Events) != 2 {
		t.Fatalf("KeepEvents retained %d events", len(tr.Events))
	}
	tr = Capture(context.Background(), stubTarget{events: events}, "k", history.Message{}, CaptureOptions{})
	if tr.Events != nil {
		t.Fatalf("events retained without KeepEvents")
	}
}
