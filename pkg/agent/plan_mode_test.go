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
		ch <- Event(ContentEvent{Text: r.Content})
	}
	return r, nil
}

func TestWithPlanModeHint_InjectsWhenActive(t *testing.T) {
	al := &AgentLoop{}
	al.SetPlanMode("s1", true)
	msgs := []history.Message{{Role: "system", Content: "you are helpful"}}
	out := al.withPlanModeHint("s1", msgs)
	if !strings.Contains(out[0].Content, planModeSentinel) {
		t.Fatalf("plan-mode sentinel not injected: %q", out[0].Content)
	}
	// Input slice must not be mutated.
	if msgs[0].Content != "you are helpful" {
		t.Fatal("input msgs mutated")
	}
}

func TestWithPlanModeHint_SkipsWhenInactive(t *testing.T) {
	al := &AgentLoop{}
	msgs := []history.Message{{Role: "system", Content: "base"}}
	out := al.withPlanModeHint("s1", msgs)
	if out[0].Content != "base" {
		t.Fatalf("hint injected while PlanMode=false: %q", out[0].Content)
	}
}

func TestWithPlanModeHint_Idempotent(t *testing.T) {
	al := &AgentLoop{}
	al.SetPlanMode("s1", true)
	msgs := []history.Message{{Role: "system", Content: "base"}}
	once := al.withPlanModeHint("s1", msgs)
	twice := al.withPlanModeHint("s1", once)
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
	loop.SetPlanMode("s1", true)

	var capturedPlan string
	loop.ConfirmPlan = func(_ context.Context, plan PlanProposal) bool {
		capturedPlan = plan.Plan
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
	if loop.IsPlanMode("s1") {
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
	loop.SetPlanMode("s1", true)
	loop.ConfirmPlan = func(_ context.Context, _ PlanProposal) bool { return false }

	if _, err := loop.RunIteration(context.Background(), "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if !loop.IsPlanMode("s1") {
		t.Fatal("PlanMode should remain true after denial")
	}

	hist, _ := sm.History(context.Background(), "s1")
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
	loop.SetPlanMode("s1", true)
	loop.ConfirmPlan = func(_ context.Context, _ PlanProposal) bool { return true }

	if _, err := loop.RunIteration(context.Background(), "s1", "start"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}

	// do_work should have run exactly once (post-approval on turn 3), not on turn 1.
	if got := len(runTool.receivedArgs()); got != 1 {
		t.Fatalf("do_work ran %d times, want 1 (turn-1 call must be blocked)", got)
	}

	// First turn should have recorded a blocked tool result for t1.
	hist, _ := sm.History(context.Background(), "s1")
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

// TestPlanMode_ConcurrentAccess pins the contract that plan mode is safe
// for concurrent access across sessions — the realistic scenario is one
// AgentLoop shared across HTTP handlers. Run under `-race` to verify.
func TestPlanMode_ConcurrentAccess(t *testing.T) {
	al := &AgentLoop{}
	var wg sync.WaitGroup
	for i := range 64 {
		key := "s" + string(rune('0'+i%10))
		wg.Add(2)
		go func() { defer wg.Done(); al.SetPlanMode(key, true) }()
		go func() { defer wg.Done(); _ = al.IsPlanMode(key) }()
	}
	wg.Wait()
}

// sessionRoutingProviderPM dispatches scripted turns by sessionKey extracted
// from ctx, so a single AgentLoop can serve multiple sessions in one test.
type sessionRoutingProviderPM struct {
	mu      sync.Mutex
	routes  map[string][]LLMResult
	cursors map[string]int
}

func (p *sessionRoutingProviderPM) GenerateStream(ctx context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	sessionKey, _ := SessionKeyFromContext(ctx)
	p.mu.Lock()
	turns := p.routes[sessionKey]
	idx := p.cursors[sessionKey]
	p.cursors[sessionKey]++
	p.mu.Unlock()
	if idx >= len(turns) {
		return LLMResult{Content: "done"}, nil
	}
	r := turns[idx]
	if len(r.ToolCalls) == 0 {
		ch <- Event(ContentEvent{Text: r.Content})
	}
	return r, nil
}

// TestPlanMode_MultiSessionLoopIsolatesApproval is the end-to-end proof that
// the wiring through RunIterationStream uses session-keyed reads. With the
// pre-fix global flag, Alice's approval would clear plan mode for everyone;
// Bob's subsequent do_work would slip through unblocked. The assertions
// here would both fail under the old design.
func TestPlanMode_MultiSessionLoopIsolatesApproval(t *testing.T) {
	tool := &recordingTool{name: "do_work", result: `"ok"`}
	provider := &sessionRoutingProviderPM{
		routes: map[string][]LLMResult{
			"alice": {
				{ToolCalls: []PendingToolCall{{ID: "tp", Name: ExitPlanModeToolName, ArgsJSON: `{"plan":"alice"}`}}},
				{ToolCalls: []PendingToolCall{{ID: "ta", Name: "do_work", ArgsJSON: `{"who":"alice"}`}}},
				{Content: "alice done"},
			},
			"bob": {
				{ToolCalls: []PendingToolCall{{ID: "tb", Name: "do_work", ArgsJSON: `{"who":"bob"}`}}},
				{Content: "bob retried"},
			},
		},
		cursors: map[string]int{},
	}

	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	reg.Register(tool)
	loop := NewAgentLoop(sm, reg, provider)
	loop.SetPlanMode("alice", true)
	loop.SetPlanMode("bob", true)
	loop.ConfirmPlan = func(_ context.Context, _ PlanProposal) bool { return true }

	// Alice runs to completion first — approves plan, then runs do_work.
	if _, err := loop.RunIteration(context.Background(), "alice", "go"); err != nil {
		t.Fatalf("alice: %v", err)
	}
	// Bob runs after Alice's approval. With the old global flag, Alice's
	// approval would have cleared plan mode for Bob too, letting his
	// do_work through. With per-session state, Bob's flag survives.
	if _, err := loop.RunIteration(context.Background(), "bob", "go"); err != nil {
		t.Fatalf("bob: %v", err)
	}

	// Alice's tool ran exactly once with her own args.
	got := tool.receivedArgs()
	if len(got) != 1 || !strings.Contains(got[0], "alice") {
		t.Fatalf("expected exactly one alice tool run, got %v", got)
	}
	// Bob's tool call must have been blocked — no invocation with "bob" args.
	for _, a := range got {
		if strings.Contains(a, "bob") {
			t.Fatalf("bob's do_work ran despite plan mode — cross-session leak: %q", a)
		}
	}
	// Bob's blocked tool call must have produced an error tool result.
	bobHist, _ := sm.History(context.Background(), "bob")
	var blocked history.Message
	for _, m := range bobHist {
		if m.Role == "tool" && m.ToolCallID == "tb" {
			blocked = m
			break
		}
	}
	if blocked.Role == "" || !blocked.IsError || !strings.Contains(blocked.Content, "blocked in plan mode") {
		t.Fatalf("expected bob's tb to be blocked in plan mode, got: %+v", blocked)
	}
	// Plan-mode flags must reflect per-session state post-approval.
	if loop.IsPlanMode("alice") {
		t.Fatal("alice should have exited plan mode after approval")
	}
	if !loop.IsPlanMode("bob") {
		t.Fatal("bob's plan mode was cleared by alice's approval — per-session isolation broken")
	}
}

// TestClearSession_RemovesPlanMode verifies the cleanup hook for abandoned
// plan-mode sessions (the leak gap noted in the original review).
func TestClearSession_RemovesPlanMode(t *testing.T) {
	al := &AgentLoop{}
	al.SetPlanMode("ghost", true)
	if !al.IsPlanMode("ghost") {
		t.Fatal("setup: ghost should be in plan mode")
	}
	al.ClearSession("ghost")
	if al.IsPlanMode("ghost") {
		t.Fatal("ClearSession should have wiped ghost's plan-mode entry")
	}
	// Idempotent.
	al.ClearSession("ghost")
	al.ClearSession("never-existed")
}

// TestPlanMode_PerSessionIsolation pins the architectural guarantee that
// plan mode is per-session: approving one session's plan must not leak
// into another. This is the regression that motivated moving from a
// global atomic.Bool to a sync.Map keyed by sessionKey.
func TestPlanMode_PerSessionIsolation(t *testing.T) {
	al := &AgentLoop{}
	al.SetPlanMode("alice", true)
	al.SetPlanMode("bob", true)

	// Alice's plan gets approved; Bob is unaffected.
	al.SetPlanMode("alice", false)

	if al.IsPlanMode("alice") {
		t.Fatal("alice should have exited plan mode")
	}
	if !al.IsPlanMode("bob") {
		t.Fatal("bob's plan mode must not be cleared by alice's approval")
	}
	if al.IsPlanMode("carol") {
		t.Fatal("carol never entered plan mode")
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
	loop.SetPlanMode("s1", true)

	if _, err := loop.RunIteration(context.Background(), "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if !loop.IsPlanMode("s1") {
		t.Fatal("PlanMode should remain true when ConfirmPlan is nil")
	}
}
