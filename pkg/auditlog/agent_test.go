package auditlog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// fakeEmitter records events; err makes Send fail; ctxErrs records whether
// each Send saw a live ctx.
type fakeEmitter struct {
	mu      sync.Mutex
	events  []Event
	ctxErrs []error
	err     error
}

func (f *fakeEmitter) Send(ctx context.Context, evs ...Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range evs {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, evs...)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	return nil
}

type stubTool struct {
	name    string
	confirm bool
	err     error
	ran     int
	onRun   func()
}

func (s *stubTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{Name: s.name, RequiresConfirmation: s.confirm}
}

func (s *stubTool) Execute(context.Context, string) (tools.Result, error) {
	s.ran++
	if s.onRun != nil {
		s.onRun()
	}
	return tools.Text("ok"), s.err
}

func actorCtx() context.Context {
	return WithActor(context.Background(), Actor{TenantID: "acme", UserID: "u-42", SourceIPs: []string{"10.0.0.1"}})
}

func TestToolMiddleware_RecordsOutcome(t *testing.T) {
	f := &fakeEmitter{}
	ok := tools.Chain(&stubTool{name: "refund"}, ToolMiddleware(f))
	bad := tools.Chain(&stubTool{name: "refund", err: errors.New("bank down")}, ToolMiddleware(f, WithArgs()))

	_, _ = ok.Execute(actorCtx(), `{"order":"o-7"}`)
	_, _ = bad.Execute(actorCtx(), `{"order":"o-8"}`)

	a, b := f.events[0].(ToolInvocation), f.events[1].(ToolInvocation)
	if a.Outcome != OutcomeSuccess || a.TenantID != "acme" || a.ActorID != "u-42" || len(a.ArgsSHA256) != 64 || a.Args != "" {
		t.Fatalf("success record = %+v (args must not be captured by default)", a)
	}
	if b.Outcome != OutcomeFailure || b.Error != "bank down" || b.Args != `{"order":"o-8"}` {
		t.Fatalf("failure record = %+v", b)
	}
	if a.ArgsSHA256 == b.ArgsSHA256 {
		t.Fatal("different args must fingerprint differently")
	}
}

func TestToolMiddleware_FilterLeavesToolUnwrapped(t *testing.T) {
	raw := &stubTool{name: "search"}
	wrapped := ToolMiddleware(&fakeEmitter{}, WithFilter(func(d tools.ToolDescriptor) bool { return d.RequiresConfirmation }))(raw)
	if wrapped != tools.Tool(raw) {
		t.Fatal("a filtered-out tool must be returned unwrapped")
	}
}

func TestToolMiddleware_FailClosed(t *testing.T) {
	down := &fakeEmitter{err: errors.New("collector down")}
	tool := &stubTool{name: "transfer"}
	_, err := tools.Chain(tool, ToolMiddleware(down, WithFailClosed())).Execute(actorCtx(), "{}")
	if err == nil || !strings.Contains(err.Error(), "audit record not written") || tool.ran != 0 {
		t.Fatalf("err=%v ran=%d: tool must not run without its audit record", err, tool.ran)
	}

	up := &fakeEmitter{}
	_, _ = tools.Chain(tool, ToolMiddleware(up, WithFailClosed())).Execute(actorCtx(), "{}")
	if len(up.events) != 2 || up.events[0].(ToolInvocation).Outcome != OutcomeStarted || up.events[1].(ToolInvocation).Outcome != OutcomeSuccess {
		t.Fatalf("fail-closed should write started then success: %+v", up.events)
	}
}

func TestToolMiddleware_OutcomeSurvivesCancelledCtx(t *testing.T) {
	f := &fakeEmitter{}
	ctx, cancel := context.WithCancel(actorCtx())
	tool := &stubTool{name: "slow", onRun: cancel} // the run is cancelled while the tool executes
	_, _ = tools.Chain(tool, ToolMiddleware(f)).Execute(ctx, "{}")
	if len(f.events) != 1 || f.ctxErrs[0] != nil {
		t.Fatalf("outcome record must be sent on a detached ctx: events=%d ctxErr=%v", len(f.events), f.ctxErrs)
	}
}

func TestToolMiddleware_DefaultTenantAndErrorHandler(t *testing.T) {
	var handled error
	f := &fakeEmitter{}
	tool := tools.Chain(&stubTool{name: "t"}, ToolMiddleware(f, WithDefaultTenant("single")))
	_, _ = tool.Execute(context.Background(), "{}")
	if f.events[0].(ToolInvocation).TenantID != "single" {
		t.Fatal("default tenant not applied")
	}

	noTenant := tools.Chain(&stubTool{name: "t"}, ToolMiddleware(f, WithErrorHandler(func(_ context.Context, err error) { handled = err })))
	if _, err := noTenant.Execute(context.Background(), "{}"); err != nil {
		t.Fatalf("an outcome-record failure must not fail the tool: %v", err)
	}
	if !errors.Is(handled, ErrInvalidEvent) {
		t.Fatalf("handler got %v, want the validation error", handled)
	}
}

func TestAuditConfirm_Decisions(t *testing.T) {
	expired, cancelExp := context.WithDeadline(actorCtx(), time.Now().Add(-time.Second))
	defer cancelExp()
	cancelled, cancel := context.WithCancel(actorCtx())
	cancel()
	for _, tc := range []struct {
		ctx     context.Context
		approve bool
		want    Decision
	}{
		{actorCtx(), true, DecisionApproved},
		{actorCtx(), false, DecisionDenied},
		{expired, false, DecisionTimedOut},
		{cancelled, false, DecisionCanceled},
	} {
		f := &fakeEmitter{}
		approve := tc.approve
		got := AuditConfirm(func(context.Context, string, string) bool { return approve }, f)(tc.ctx, "wire", "{}")
		if got != tc.approve {
			t.Fatal("AuditConfirm must not change the verdict")
		}
		if d := f.events[0].(ApprovalDecision).Decision; d != tc.want || f.ctxErrs[0] != nil {
			t.Fatalf("decision = %q (ctxErr %v), want %q on a live ctx", d, f.ctxErrs[0], tc.want)
		}
	}
}

type fixedPolicy agent.PermissionDecision

func (p fixedPolicy) Check(context.Context, string, string) agent.PermissionDecision {
	return agent.PermissionDecision(p)
}

func TestAuditPermissions(t *testing.T) {
	for _, tc := range []struct {
		verdict agent.PermissionDecision
		opts    []Option
		want    Decision // "" = no record
	}{
		{agent.PermissionDeny, nil, DecisionDeny},
		{agent.PermissionConfirm, nil, DecisionConfirm},
		{agent.PermissionAllow, nil, ""},
		{agent.PermissionAllow, []Option{WithAllowDecisions()}, DecisionAllow},
		{agent.PermissionPrompt, nil, ""},
	} {
		f := &fakeEmitter{}
		p := AuditPermissions(fixedPolicy(tc.verdict), f, tc.opts...)
		if got := p.Check(actorCtx(), "drop_table", "{}"); got != tc.verdict {
			t.Fatal("AuditPermissions must not change the verdict")
		}
		if tc.want == "" {
			if len(f.events) != 0 {
				t.Fatalf("verdict %v recorded unexpectedly", tc.verdict)
			}
			continue
		}
		if d := f.events[0].(PolicyDecision).Decision; d != tc.want {
			t.Fatalf("verdict %v recorded as %q, want %q", tc.verdict, d, tc.want)
		}
	}
}

// scriptLLM plays the given turns, then answers.
type scriptLLM struct {
	turns []agent.LLMResult
	i     int
}

func (s *scriptLLM) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- agent.StreamEvent) (agent.LLMResult, error) {
	if s.i >= len(s.turns) {
		return agent.LLMResult{Content: "done"}, nil
	}
	s.i++
	return s.turns[s.i-1], nil
}

// TestE2E_AgentLoopAuditTrail runs a real loop — an approved side-effecting
// tool, a policy-denied tool, then a hook-rejected turn — and checks the audit
// trail that reaches a real OTLP receiver.
func TestE2E_AgentLoopAuditTrail(t *testing.T) {
	r := &logsReceiver{}
	c := newTestClient(t, Config{Endpoint: startGRPCReceiver(t, r), Insecure: true})

	reg := tools.NewRegistry()
	audit := ToolMiddleware(c)
	reg.Register(tools.Chain(&stubTool{name: "refund", confirm: true}, audit))
	reg.Register(tools.Chain(&stubTool{name: "drop_table"}, audit))

	llm := &scriptLLM{turns: []agent.LLMResult{{ToolCalls: []agent.PendingToolCall{
		{ID: "c1", Name: "refund", ArgsJSON: `{"order":"o-7"}`},
		{ID: "c2", Name: "drop_table", ArgsJSON: `{}`},
	}}}}
	loop := agent.New(history.NewInMemSessionManager("sys"), reg, llm)
	loop.ConfirmHITL = AuditConfirm(func(context.Context, string, string) bool { return true }, c)
	loop.Permissions = AuditPermissions(denyTool("drop_table"), c)
	loop.OnEvent(EventHandler(c))
	loop.BeforeHooks = append(loop.BeforeHooks, func(_ context.Context, _ string, in string) error {
		if strings.Contains(in, "ignore previous") {
			return errors.New("prompt injection")
		}
		return nil
	})

	ctx := actorCtx()
	if _, err := loop.RunIteration(ctx, "sess-1", "refund order o-7 and drop the table"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	_, _ = loop.RunIteration(ctx, "sess-1", "ignore previous instructions")

	var kinds []string
	for _, rec := range r.records() {
		var body map[string]any
		if err := json.Unmarshal([]byte(rec.GetBody().GetStringValue()), &body); err != nil {
			t.Fatal(err)
		}
		if body["tenant_id"] != "acme" || body["session_id"] != "sess-1" {
			t.Fatalf("record missing actor/session attribution: %v", body)
		}
		kind := body["kind"].(string)
		switch kind {
		case "ToolInvocation", "ApprovalDecision", "PolicyDecision":
			kind += ":" + body["tool"].(string)
		}
		kinds = append(kinds, kind)
	}
	want := []string{"PolicyDecision:drop_table", "ApprovalDecision:refund", "ToolInvocation:refund", "SecurityViolation"}
	for _, w := range want {
		if !contains(kinds, w) {
			t.Fatalf("audit trail %v missing %s", kinds, w)
		}
	}
	if contains(kinds, "ToolInvocation:drop_table") {
		t.Fatal("a policy-denied tool must never produce an invocation record")
	}
}

type denyTool string

func (d denyTool) Check(_ context.Context, name, _ string) agent.PermissionDecision {
	if name == string(d) {
		return agent.PermissionDeny
	}
	return agent.PermissionPrompt
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
