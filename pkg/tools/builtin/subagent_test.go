package builtin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// capturingProvider records every history slice it receives so tests can
// verify the sub-agent's isolated session state.
type capturingProvider struct {
	mu     sync.Mutex
	inputs [][]history.Message
	reply  string
	err    error
}

func (p *capturingProvider) GenerateStream(_ context.Context, msgs []history.Message, _ *tools.Registry, ch chan<- agent.StreamEvent) (agent.LLMResult, error) {
	p.mu.Lock()
	snapshot := make([]history.Message, len(msgs))
	copy(snapshot, msgs)
	p.inputs = append(p.inputs, snapshot)
	p.mu.Unlock()

	if p.err != nil {
		return agent.LLMResult{}, p.err
	}
	ch <- agent.StreamEvent{Type: "content", Content: p.reply}
	return agent.LLMResult{Content: p.reply}, nil
}

// spySessionManager wraps an InMemSessionManager and records DeleteSession calls.
type spySessionManager struct {
	*history.InMemSessionManager
	mu      sync.Mutex
	deletes []string
}

func newSpySessionManager(systemPrompt string) *spySessionManager {
	return &spySessionManager{
		InMemSessionManager: history.NewInMemSessionManager(systemPrompt),
	}
}

func (s *spySessionManager) DeleteSession(ctx context.Context, sessionKey string) error {
	s.mu.Lock()
	s.deletes = append(s.deletes, sessionKey)
	s.mu.Unlock()
	return s.InMemSessionManager.DeleteSession(ctx, sessionKey)
}

func (s *spySessionManager) deletedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.deletes))
	copy(out, s.deletes)
	return out
}

func TestCallSubAgentTool_IsolatedSession(t *testing.T) {
	sm := newSpySessionManager("main-sys")

	sm.SetHistory(context.Background(), "parent", []history.Message{
		{Role: "system", Content: "main-sys"},
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	})

	prov := &capturingProvider{reply: "subagent reply"}
	tool := NewCallSubAgentTool(sm, tools.NewRegistry(), prov)

	_, err := tool.Execute(context.Background(), `{"task_description":"do the thing","agent_name":"worker"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if len(prov.inputs) != 1 {
		t.Fatalf("expected LLM called once, got %d", len(prov.inputs))
	}
	// Worker should see only [system, user:task] — not the parent's 3 messages.
	seen := prov.inputs[0]
	if len(seen) != 2 {
		t.Fatalf("expected 2-message pristine history, got %d: %+v", len(seen), seen)
	}
	if seen[0].Role != "system" {
		t.Fatalf("expected first message to be system, got %q", seen[0].Role)
	}
	if seen[1].Role != "user" || !strings.Contains(seen[1].Content, "do the thing") {
		t.Fatalf("expected user message with task, got: %+v", seen[1])
	}
}

func TestCallSubAgentTool_CleansUpOnSuccess(t *testing.T) {
	sm := newSpySessionManager("sys")
	prov := &capturingProvider{reply: "done"}
	tool := NewCallSubAgentTool(sm, tools.NewRegistry(), prov)

	if _, err := tool.Execute(context.Background(), `{"task_description":"x","agent_name":"w"}`); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	deleted := sm.deletedKeys()
	if len(deleted) != 1 {
		t.Fatalf("expected exactly 1 DeleteSession call, got %d: %v", len(deleted), deleted)
	}
	if !strings.HasPrefix(deleted[0], "subagent-w-") {
		t.Fatalf("expected deleted key to match worker pattern, got %q", deleted[0])
	}
}

func TestCallSubAgentTool_CleansUpOnError(t *testing.T) {
	sm := newSpySessionManager("sys")
	prov := &capturingProvider{err: errors.New("llm exploded")}
	tool := NewCallSubAgentTool(sm, tools.NewRegistry(), prov)

	_, err := tool.Execute(context.Background(), `{"task_description":"x","agent_name":"w"}`)
	if err == nil {
		t.Fatal("expected error from failing sub-agent")
	}

	deleted := sm.deletedKeys()
	if len(deleted) != 1 {
		t.Fatalf("expected cleanup even on error, got %d deletes: %v", len(deleted), deleted)
	}
}

func TestCallSubAgentTool_ForwardsEventsWhenEmitterPresent(t *testing.T) {
	sm := newSpySessionManager("sys")
	prov := &capturingProvider{reply: "worker report"}
	tool := NewCallSubAgentTool(sm, tools.NewRegistry(), prov)

	var forwarded []agent.StreamEvent
	var mu sync.Mutex
	ctx := agent.WithSubAgentEmitter(context.Background(), func(ev agent.StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		forwarded = append(forwarded, ev)
	})
	ctx = context.WithValue(ctx, agent.SessionKeyCtx("sessionKey"), "parent-session")

	result, err := tool.Execute(ctx, `{"task_description":"go","agent_name":"researcher"}`)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(result, "worker report") {
		t.Fatalf("expected worker content in report, got %q", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(forwarded) == 0 {
		t.Fatal("expected at least one forwarded event, got none")
	}

	var sawContent, sawDone bool
	for _, ev := range forwarded {
		if ev.Source != "subagent:researcher" {
			t.Fatalf("forwarded event must carry Source=subagent:researcher, got %q in %+v", ev.Source, ev)
		}
		if ev.ParentID != "parent-session" {
			t.Fatalf("forwarded event must carry ParentID=parent-session, got %q", ev.ParentID)
		}
		switch ev.Type {
		case "content":
			if ev.Content == "worker report" {
				sawContent = true
			}
		case "done":
			sawDone = true
		}
	}
	if !sawContent {
		t.Fatalf("expected a forwarded content event with the worker's reply, got %+v", forwarded)
	}
	if !sawDone {
		t.Fatalf("expected a forwarded 'done' event signalling worker finished, got %+v", forwarded)
	}
}

func TestCallSubAgentTool_NonStreamingFallbackWhenNoEmitter(t *testing.T) {
	// Without an emitter in ctx the tool must keep its original blocking
	// behavior: return the full report and do not crash trying to forward.
	sm := newSpySessionManager("sys")
	prov := &capturingProvider{reply: "legacy path"}
	tool := NewCallSubAgentTool(sm, tools.NewRegistry(), prov)

	res, err := tool.Execute(context.Background(), `{"task_description":"x","agent_name":"w"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res, "legacy path") {
		t.Fatalf("expected legacy path reply, got %q", res)
	}
}

func TestCallSubAgentTool_InvalidJSON(t *testing.T) {
	sm := newSpySessionManager("sys")
	tool := NewCallSubAgentTool(sm, tools.NewRegistry(), &capturingProvider{reply: "x"})

	_, err := tool.Execute(context.Background(), `{not json`)
	if err == nil {
		t.Fatal("expected error for invalid json")
	}
	if !strings.Contains(err.Error(), "tools: invalid json") {
		t.Fatalf("expected 'tools: invalid json' prefix, got: %v", err)
	}
}
