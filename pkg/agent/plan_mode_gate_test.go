package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// silentChan returns a chan large enough to not block on the few events
// the gate emits during a test.
func silentChan(t *testing.T) chan StreamEvent {
	t.Helper()
	return make(chan StreamEvent, 16)
}

func TestRunPlanModeGate_NotInPlanMode_PassThrough(t *testing.T) {
	al := &AgentLoop{Tools: tools.NewRegistry()}
	ws := newWaveState(1)
	tc := PendingToolCall{ID: "c1", Name: "do_work", ArgsJSON: `{}`}

	if al.runPlanModeGate(context.Background(), "s1", silentChan(t), ws, tc) {
		t.Fatal("gate must return handled=false when plan mode is off")
	}
	if len(ws.toolMsgs) != 0 {
		t.Fatalf("no tool result should be written; got %v", ws.toolMsgs)
	}
}

func TestRunPlanModeGate_BlocksNonExitToolInPlanMode(t *testing.T) {
	al := &AgentLoop{Tools: tools.NewRegistry()}
	al.SetPlanMode("s1", true)
	ws := newWaveState(1)
	tc := PendingToolCall{ID: "c1", Name: "do_work", ArgsJSON: `{}`}

	if !al.runPlanModeGate(context.Background(), "s1", silentChan(t), ws, tc) {
		t.Fatal("expected handled=true for non-exit tool in plan mode")
	}
	got, ok := ws.toolMsgs[tc.ID]
	if !ok {
		t.Fatal("expected tool result for blocked call")
	}
	if !got.IsError || !strings.Contains(got.Content, "blocked in plan mode") {
		t.Fatalf("unexpected blocked message: %+v", got)
	}
	if _, ok := ws.resultsByID[tc.ID]; ok {
		t.Fatal("blocked call must not appear in resultsByID")
	}
}

func TestRunPlanModeGate_ApprovedExitsPlanModeAndPublishesResult(t *testing.T) {
	al := &AgentLoop{Tools: tools.NewRegistry()}
	al.SetPlanMode("s1", true)
	al.ConfirmPlan = func(_ context.Context, _ PlanProposal) bool { return true }
	ws := newWaveState(1)
	tc := PendingToolCall{ID: "tp", Name: ExitPlanModeToolName, ArgsJSON: `{"plan":"step a"}`}

	if !al.runPlanModeGate(context.Background(), "s1", silentChan(t), ws, tc) {
		t.Fatal("expected handled=true on approval")
	}
	if al.IsPlanMode("s1") {
		t.Fatal("plan mode should have been cleared after approval")
	}
	got := ws.toolMsgs[tc.ID]
	if got.IsError {
		t.Fatalf("approved result should not be error: %+v", got)
	}
	if _, ok := ws.resultsByID[tc.ID]; !ok {
		t.Fatal("approved exit_plan_mode result must publish to resultsByID")
	}
}

func TestRunPlanModeGate_PassesRawArgsToConfirmPlan(t *testing.T) {
	// A host that registers a structured exit_plan_mode tool gets the
	// untouched tool-call JSON on PlanProposal.RawArgs, so it can unmarshal
	// typed steps without parsing markdown. Plan stays empty when the schema
	// has no top-level string `plan` field.
	al := &AgentLoop{Tools: tools.NewRegistry()}
	al.SetPlanMode("s1", true)
	var got PlanProposal
	al.ConfirmPlan = func(_ context.Context, p PlanProposal) bool {
		got = p
		return true
	}
	ws := newWaveState(1)
	const structured = `{"goal":"reel","steps":[{"op":"cut","at":1.5}]}`
	tc := PendingToolCall{ID: "tp", Name: ExitPlanModeToolName, ArgsJSON: structured}

	if !al.runPlanModeGate(context.Background(), "s1", silentChan(t), ws, tc) {
		t.Fatal("expected handled=true on approval")
	}
	if string(got.RawArgs) != structured {
		t.Fatalf("RawArgs not passed through verbatim: got %q want %q", got.RawArgs, structured)
	}
	if got.Plan != "" {
		t.Fatalf("Plan should be empty for a structured schema with no `plan` field; got %q", got.Plan)
	}
	var parsed struct {
		Goal  string `json:"goal"`
		Steps []struct {
			Op string  `json:"op"`
			At float64 `json:"at"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(got.RawArgs, &parsed); err != nil {
		t.Fatalf("unmarshal RawArgs: %v", err)
	}
	if parsed.Goal != "reel" || len(parsed.Steps) != 1 || parsed.Steps[0].Op != "cut" {
		t.Fatalf("structured plan did not round-trip from RawArgs: %+v", parsed)
	}
}

func TestRunPlanModeGate_DeniedKeepsPlanModeAndFlagsError(t *testing.T) {
	al := &AgentLoop{Tools: tools.NewRegistry()}
	al.SetPlanMode("s1", true)
	al.ConfirmPlan = func(_ context.Context, _ PlanProposal) bool { return false }
	ws := newWaveState(1)
	tc := PendingToolCall{ID: "tp", Name: ExitPlanModeToolName, ArgsJSON: `{"plan":"x"}`}

	if !al.runPlanModeGate(context.Background(), "s1", silentChan(t), ws, tc) {
		t.Fatal("expected handled=true on denial")
	}
	if !al.IsPlanMode("s1") {
		t.Fatal("plan mode must remain after denial")
	}
	got := ws.toolMsgs[tc.ID]
	if !got.IsError {
		t.Fatalf("denied result must be flagged as error: %+v", got)
	}
	if _, ok := ws.resultsByID[tc.ID]; ok {
		t.Fatal("denied result must not publish to resultsByID")
	}
}
