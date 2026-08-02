package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/cache"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// halfSuccessTool writes its "artifact" fine and always fails the derived
// bookkeeping, which is the exact shape DegradedEvent exists for.
type halfSuccessTool struct {
	name      string
	err       error
	cacheable bool
}

func (t *halfSuccessTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        t.name,
		Description: "writes a report and updates the index",
		Parameters:  tools.ToolSchema{Type: "object"},
		Cacheable:   t.cacheable,
	}
}

func (t *halfSuccessTool) Execute(context.Context, string) (tools.Result, error) {
	// The degradation rides along even on the error branch: a tool that
	// wrote its artifact and then failed hard still left the artifact
	// behind. Whether that gets reported is the loop's decision, and it
	// depends on the post-hook error state — which is exactly what the
	// speculative and non-speculative paths must agree on.
	res := tools.Result{
		Text: "report written to /reports/q3.md",
		Degraded: &tools.Degradation{
			Reason:     "report written but the search index update failed",
			Artifacts:  []string{"/reports/q3.md"},
			Unreliable: []string{"search_index"},
		},
	}
	if t.err != nil {
		return res, t.err
	}
	return res, nil
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

func TestDegraded_ParallelWaveCollectsEveryUnit(t *testing.T) {
	// Three half-success tools in one wave exercise degradedAcc's mutex.
	// Run under -race; an unguarded append would be caught here.
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{
			{ID: "c1", Name: "w1", ArgsJSON: `{}`},
			{ID: "c2", Name: "w2", ArgsJSON: `{}`},
			{ID: "c3", Name: "w3", ArgsJSON: `{}`},
		}},
		{Content: "all three ran"},
	}}
	loop, _ := setup(provider,
		&halfSuccessTool{name: "w1"}, &halfSuccessTool{name: "w2"}, &halfSuccessTool{name: "w3"})

	var units []ToolDegradation
	for _, ev := range drainEvents(t, loop, "s1", "run all three") {
		if p, ok := ev.Payload.(DegradedEvent); ok {
			units = append(units, p.Units...)
		}
	}
	if len(units) != 3 {
		t.Fatalf("want 3 degradations from a 3-tool wave, got %d: %+v", len(units), units)
	}
	seen := map[string]bool{}
	for _, u := range units {
		seen[u.Tool] = true
	}
	for _, name := range []string{"w1", "w2", "w3"} {
		if !seen[name] {
			t.Fatalf("missing degradation from %s: %+v", name, units)
		}
	}
}

func TestDegraded_NotCachedSoAReplayCannotFakeSuccess(t *testing.T) {
	// A cacheable tool that degrades must not populate the cache: a hit
	// would replay the partial-success note to a later turn's model while
	// the host sees no DegradedEvent at all.
	tool := &halfSuccessTool{name: "write_report", cacheable: true}
	newRun := func() (*AgentLoop, *cache.SearchCache) {
		c := cache.NewSearchCache(10, time.Minute)
		provider := &scriptProvider{turns: []LLMResult{
			{ToolCalls: []PendingToolCall{{ID: "c1", Name: "write_report", ArgsJSON: `{}`}}},
			{Content: "done"},
		}}
		loop, _ := setup(provider, tool)
		loop.Cache = c
		return loop, c
	}

	loop, shared := newRun()
	if evs := degradedUnits(drainEvents(t, loop, "s1", "write it")); len(evs) != 1 {
		t.Fatalf("first run: want 1 degradation, got %d", len(evs))
	}

	// Second run reuses the same cache; the tool must execute again.
	provider2 := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "write_report", ArgsJSON: `{}`}}},
		{Content: "done"},
	}}
	loop2, _ := setup(provider2, tool)
	loop2.Cache = shared
	if evs := degradedUnits(drainEvents(t, loop2, "s2", "write it")); len(evs) != 1 {
		t.Fatalf("second run served a degraded result from cache — host saw %d degradations, want 1", len(evs))
	}
}

// degradedUnits flattens every DegradedEvent payload in evs.
func degradedUnits(evs []StreamEvent) []ToolDegradation {
	var out []ToolDegradation
	for _, ev := range evs {
		if p, ok := ev.Payload.(DegradedEvent); ok {
			out = append(out, p.Units...)
		}
	}
	return out
}

// --- speculative execution ---

func TestDegraded_SpeculatedResultFiledExactlyOnce(t *testing.T) {
	// The tool runs in the drainer's speculation goroutine and its result
	// is consumed by the wave executor. Exactly one filer must claim it:
	// double-filing would show the operator two failures for one call.
	loop, _ := setup(
		&streamingToolCallReadyProvider{toolName: "write_report", argsJSON: `{}`},
		&halfSuccessTool{name: "write_report"},
	)
	loop.SpeculativeTools = true

	units := degradedUnits(drainEvents(t, loop, "s1", "write it"))
	if len(units) != 1 {
		t.Fatalf("want exactly 1 degradation from a speculated call, got %d: %+v", len(units), units)
	}
	if units[0].Tool != "write_report" {
		t.Fatalf("unit = %+v, want write_report", units[0])
	}
}

func TestDegraded_SpeculatedResultRecoveredByHookIsStillFiled(t *testing.T) {
	// OnToolResult can recover an errored call into a success. The
	// degradation must be judged against the POST-hook state: deciding at
	// execution time reports nothing here, so the model is told the work
	// half-landed while the host sees a clean turn.
	loop, _ := setup(
		&streamingToolCallReadyProvider{toolName: "write_report", argsJSON: `{}`},
		&halfSuccessTool{name: "write_report", err: errors.New("index update failed")},
	)
	loop.SpeculativeTools = true
	loop.OnToolResult = func(_ context.Context, _, _, _, _ string, _ any, _ error) (string, error) {
		return "report written, index repair queued", nil // error in -> recovered
	}

	units := degradedUnits(drainEvents(t, loop, "s1", "write it"))
	if len(units) != 1 {
		t.Fatalf("hook recovered the call, so the degradation must be reported; got %d: %+v", len(units), units)
	}
}
