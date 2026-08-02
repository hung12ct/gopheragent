package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// halfSuccessTool writes its "artifact" fine and always fails the derived
// bookkeeping, which is the exact shape DegradedEvent exists for.
type halfSuccessTool struct {
	name string
	err  error
}

func (t *halfSuccessTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        t.name,
		Description: "writes a report and updates the index",
		Parameters:  tools.ToolSchema{Type: "object"},
	}
}

func (t *halfSuccessTool) Execute(context.Context, string) (tools.Result, error) {
	if t.err != nil {
		return tools.Result{}, t.err
	}
	return tools.Result{
		Text: "report written to /reports/q3.md",
		Degraded: &tools.Degradation{
			Reason:     "report written but the search index update failed",
			Artifacts:  []string{"/reports/q3.md"},
			Unreliable: []string{"search_index"},
		},
	}, nil
}

// drainEvents runs one streaming turn and returns every event emitted.
func drainEvents(t *testing.T, loop *AgentLoop, sessionKey, msg string) []StreamEvent {
	t.Helper()
	var evs []StreamEvent
	for ev := range loop.RunText(context.Background(), sessionKey, msg) {
		evs = append(evs, ev)
	}
	return evs
}

func TestDegraded_EmittedBeforeDoneOnFinalAnswer(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "write_report", ArgsJSON: `{}`}}},
		{Content: "the report is ready"},
	}}
	loop, _ := setup(provider, &halfSuccessTool{name: "write_report"})

	evs := drainEvents(t, loop, "s1", "write the q3 report")

	degradedAt, doneAt := -1, -1
	var payload DegradedEvent
	for i, ev := range evs {
		switch p := ev.Payload.(type) {
		case DegradedEvent:
			degradedAt, payload = i, p
		case DoneEvent:
			doneAt = i
		}
	}
	if degradedAt < 0 {
		t.Fatal("no DegradedEvent emitted for a tool that reported partial success")
	}
	if doneAt < 0 {
		t.Fatal("DoneEvent must still fire — a degraded turn produced a real answer")
	}
	if degradedAt > doneAt {
		t.Fatalf("DegradedEvent must precede DoneEvent, got indices %d and %d", degradedAt, doneAt)
	}
	if len(payload.Units) != 1 {
		t.Fatalf("units = %+v, want exactly one", payload.Units)
	}
	u := payload.Units[0]
	if u.Tool != "write_report" || len(u.Artifacts) != 1 || len(u.Unreliable) != 1 {
		t.Fatalf("unit lost its detail: %+v", u)
	}
	if !errors.Is(payload.Err, ErrDegraded) {
		t.Fatalf("Err = %v, want errors.Is(..., ErrDegraded)", payload.Err)
	}
}

func TestDegraded_NoteReachesTheModel(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "write_report", ArgsJSON: `{}`}}},
		{Content: "done"},
	}}
	loop, sm := setup(provider, &halfSuccessTool{name: "write_report"})

	if _, err := loop.RunIteration(context.Background(), "s1", "write it"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs, err := sm.History(context.Background(), "s1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var toolContent string
	for _, m := range msgs {
		if m.Role == "tool" {
			toolContent = m.Content
		}
	}
	if !strings.Contains(toolContent, "[System: partial success]") {
		t.Fatalf("tool result should carry the partial-success note, got %q", toolContent)
	}
	if !strings.Contains(toolContent, "/reports/q3.md") || !strings.Contains(toolContent, "search_index") {
		t.Fatalf("note should name the artifact and the unreliable state, got %q", toolContent)
	}
}

func TestDegraded_SilentWhenNoToolDegrades(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{{Content: "nothing to do"}}}
	loop, _ := setup(provider)

	for _, ev := range drainEvents(t, loop, "s1", "hi") {
		if _, ok := ev.Payload.(DegradedEvent); ok {
			t.Fatal("DegradedEvent must not fire on a clean turn")
		}
	}
}

func TestDegraded_ToolErrorSuppressesDegradation(t *testing.T) {
	// A tool that errors is not degraded — it failed, and the error path
	// already tells that story.
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "write_report", ArgsJSON: `{}`}}},
		{Content: "could not write it"},
	}}
	loop, _ := setup(provider, &halfSuccessTool{name: "write_report", err: errors.New("disk full")})

	for _, ev := range drainEvents(t, loop, "s1", "write it") {
		if _, ok := ev.Payload.(DegradedEvent); ok {
			t.Fatal("a failed tool call must not report a partial success")
		}
	}
}

func TestDegraded_ReportedOnMaxItersTerminal(t *testing.T) {
	// The Run degrades and then never reaches a final answer. The
	// deferred sweep must still report the unreliable state.
	loop, _ := setup(&scriptProvider{turns: []LLMResult{}}, &halfSuccessTool{name: "write_report"})
	loop.MaxIters = 2
	loop.LLM = &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "write_report", ArgsJSON: `{}`}}},
		{ToolCalls: []PendingToolCall{{ID: "c2", Name: "write_report", ArgsJSON: `{"n":2}`}}},
	}}

	var seen bool
	for _, ev := range drainEvents(t, loop, "s1", "write it") {
		if _, ok := ev.Payload.(DegradedEvent); ok {
			seen = true
		}
	}
	if !seen {
		t.Fatal("a Run that degraded and then hit MaxIters must still report it")
	}
}

func TestDegradedAcc_DrainIsIdempotent(t *testing.T) {
	acc := &degradedAcc{}
	acc.add(ToolDegradation{Tool: "a"})
	if got := acc.drain(); len(got) != 1 {
		t.Fatalf("first drain = %+v, want one unit", got)
	}
	if got := acc.drain(); len(got) != 0 {
		t.Fatalf("second drain = %+v, want empty so terminals cannot double-emit", got)
	}
}
