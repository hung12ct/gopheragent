package agent

import (
	"context"
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
	al.ConfirmPlan = func(_ context.Context, _ string) bool { return true }
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

func TestRunPlanModeGate_DeniedKeepsPlanModeAndFlagsError(t *testing.T) {
	al := &AgentLoop{Tools: tools.NewRegistry()}
	al.SetPlanMode("s1", true)
	al.ConfirmPlan = func(_ context.Context, _ string) bool { return false }
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
