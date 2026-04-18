package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// systemCapturingProviderPM captures the system prompt the loop sends to the
// LLM so tests can assert plan-mode hint injection. It also honors a scripted
// sequence of turns.
type systemCapturingProviderPM struct {
	turns []LLMResult
	idx   int

	mu         sync.Mutex
	capturedSP []string
}

func (p *systemCapturingProviderPM) GenerateStream(_ context.Context, msgs []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.mu.Lock()
	sp := ""
	for _, m := range msgs {
		if m.Role == "system" {
			sp = m.Content
			break
		}
	}
	p.capturedSP = append(p.capturedSP, sp)
	p.mu.Unlock()

	if p.idx >= len(p.turns) {
		return LLMResult{Content: "done"}, nil
	}
	r := p.turns[p.idx]
	p.idx++
	if len(r.ToolCalls) == 0 {
		ch <- StreamEvent{Type: "content", Content: r.Content}
	}
	return r, nil
}

func TestWithPlanModeHint_InjectsWhenActive(t *testing.T) {
	al := &AgentLoop{PlanMode: true}
	msgs := []history.Message{{Role: "system", Content: "you are helpful"}}
	out := al.withPlanModeHint(msgs)
	if !strings.Contains(out[0].Content, planModeSentinel) {
		t.Fatalf("plan-mode sentinel not injected: %q", out[0].Content)
	}
	// Input slice must not be mutated.
	if msgs[0].Content != "you are helpful" {
		t.Fatal("input msgs mutated")
	}
}

func TestWithPlanModeHint_SkipsWhenInactive(t *testing.T) {
	al := &AgentLoop{PlanMode: false}
	msgs := []history.Message{{Role: "system", Content: "base"}}
	out := al.withPlanModeHint(msgs)
	if out[0].Content != "base" {
		t.Fatalf("hint injected while PlanMode=false: %q", out[0].Content)
	}
}

func TestWithPlanModeHint_Idempotent(t *testing.T) {
	al := &AgentLoop{PlanMode: true}
	msgs := []history.Message{{Role: "system", Content: "base"}}
	once := al.withPlanModeHint(msgs)
	twice := al.withPlanModeHint(once)
	if strings.Count(twice[0].Content, planModeSentinel) != 1 {
		t.Fatalf("sentinel appears %d times, want 1: %q",
			strings.Count(twice[0].Content, planModeSentinel), twice[0].Content)
	}
}

func TestPlanMode_ApprovedPlanExitsModeAndContinues(t *testing.T) {
	// Turn 1: model calls exit_plan_mode with a plan.
	// Turn 2: model calls a real tool (now allowed).
	// Turn 3: model finalizes.
	runTool := &recordingTool{name: "do_work", result: `"done"`}
	provider := &systemCapturingProviderPM{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "tp", Name: ExitPlanModeToolName, ArgsJSON: `{"plan":"1. audit\n2. rewrite"}`}}},
		{ToolCalls: []PendingToolCall{{ID: "t1", Name: "do_work", ArgsJSON: `{}`}}},
		{Content: "finished"},
	}}

	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	reg.Register(runTool)
	loop := NewAgentLoop(sm, reg, provider)
	loop.PlanMode = true

	var capturedPlan string
	loop.ConfirmPlan = func(_ context.Context, plan string) bool {
		capturedPlan = plan
		return true
	}

	resp, err := loop.RunIteration(context.Background(), "s1", "refactor the module")
	if err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if !strings.Contains(resp, "finished") {
		t.Fatalf("expected 'finished', got %q", resp)
	}
	if !strings.Contains(capturedPlan, "rewrite") {
		t.Fatalf("plan not passed to ConfirmPlan: %q", capturedPlan)
	}
	if loop.PlanMode {
		t.Fatal("PlanMode should be cleared after approval")
	}
	if len(runTool.receivedArgs()) != 1 {
		t.Fatalf("real tool should have run once post-approval, got %d", len(runTool.receivedArgs()))
	}
	// System prompt on turn 1 should carry the plan-mode hint; turn 2 should not.
	if !strings.Contains(provider.capturedSP[0], planModeSentinel) {
		t.Fatalf("turn 1 system prompt missing plan-mode hint: %q", provider.capturedSP[0])
	}
	if len(provider.capturedSP) >= 2 && strings.Contains(provider.capturedSP[1], planModeSentinel) {
		t.Fatalf("turn 2 should not carry plan-mode hint after approval: %q", provider.capturedSP[1])
	}
}

func TestPlanMode_DeniedPlanStaysInModeAndBlocksTools(t *testing.T) {
	// Model calls exit_plan_mode, user denies, model then calls finalize.
	provider := &systemCapturingProviderPM{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "tp", Name: ExitPlanModeToolName, ArgsJSON: `{"plan":"bad plan"}`}}},
		{Content: "I will revise and try again."},
	}}

	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	loop := NewAgentLoop(sm, reg, provider)
	loop.PlanMode = true
	loop.ConfirmPlan = func(_ context.Context, _ string) bool { return false }

	if _, err := loop.RunIteration(context.Background(), "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if !loop.PlanMode {
		t.Fatal("PlanMode should remain true after denial")
	}

	hist := sm.GetHistory(context.Background(), "s1")
	var toolMsg history.Message
	for _, m := range hist {
		if m.Role == "tool" && m.ToolCallID == "tp" {
			toolMsg = m
			break
		}
	}
	if toolMsg.Role == "" {
		t.Fatal("expected tool result for denied plan")
	}
	if !toolMsg.IsError || !strings.Contains(toolMsg.Content, "User rejected") {
		t.Fatalf("denial message unexpected: %+v", toolMsg)
	}
}

func TestPlanMode_BlocksOtherToolsAndAllowsExitPlanMode(t *testing.T) {
	// Model wrongly tries do_work first while in plan mode; loop blocks it.
	// Second turn the model corrects by calling exit_plan_mode.
	runTool := &recordingTool{name: "do_work", result: `"ok"`}
	provider := &systemCapturingProviderPM{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "t1", Name: "do_work", ArgsJSON: `{}`}}},
		{ToolCalls: []PendingToolCall{{ID: "tp", Name: ExitPlanModeToolName, ArgsJSON: `{"plan":"plan"}`}}},
		{ToolCalls: []PendingToolCall{{ID: "t2", Name: "do_work", ArgsJSON: `{}`}}},
		{Content: "ok"},
	}}

	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	reg.Register(runTool)
	loop := NewAgentLoop(sm, reg, provider)
	loop.PlanMode = true
	loop.ConfirmPlan = func(_ context.Context, _ string) bool { return true }

	if _, err := loop.RunIteration(context.Background(), "s1", "start"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}

	// do_work should have run exactly once (post-approval on turn 3), not on turn 1.
	if got := len(runTool.receivedArgs()); got != 1 {
		t.Fatalf("do_work ran %d times, want 1 (turn-1 call must be blocked)", got)
	}

	// First turn should have recorded a blocked tool result for t1.
	hist := sm.GetHistory(context.Background(), "s1")
	var blocked history.Message
	for _, m := range hist {
		if m.Role == "tool" && m.ToolCallID == "t1" {
			blocked = m
			break
		}
	}
	if blocked.Role == "" {
		t.Fatal("no tool result recorded for blocked t1")
	}
	if !blocked.IsError || !strings.Contains(blocked.Content, "blocked in plan mode") {
		t.Fatalf("t1 should be blocked, got: %+v", blocked)
	}
}

func TestPlanMode_AutoDeniesWhenConfirmPlanNil(t *testing.T) {
	// No ConfirmPlan callback + PlanMode=true → exit_plan_mode is auto-denied
	// (action_required emitted) and PlanMode stays active.
	provider := &systemCapturingProviderPM{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "tp", Name: ExitPlanModeToolName, ArgsJSON: `{"plan":"x"}`}}},
		{Content: "awaiting approval"},
	}}

	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	loop := NewAgentLoop(sm, reg, provider)
	loop.PlanMode = true

	if _, err := loop.RunIteration(context.Background(), "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if !loop.PlanMode {
		t.Fatal("PlanMode should remain true when ConfirmPlan is nil")
	}
}
