package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func newLoopWithTools(n int) *AgentLoop {
	reg := tools.NewRegistry()
	for i := range n {
		reg.Register(&echoTool{name: "tool_" + string(rune('a'+i))})
	}
	return &AgentLoop{Tools: reg}
}

func TestWithToolChainingHint_SkipsWhenFewerThanTwoTools(t *testing.T) {
	msgs := []history.Message{{Role: "system", Content: "base"}}

	for _, n := range []int{0, 1} {
		loop := newLoopWithTools(n)
		got := loop.withToolChainingHint(msgs)
		if len(got) != 1 || got[0].Content != "base" {
			t.Fatalf("n=%d: expected no injection, got %+v", n, got)
		}
	}
}

func TestWithToolChainingHint_AppendsToExistingSystemMessage(t *testing.T) {
	loop := newLoopWithTools(2)
	msgs := []history.Message{
		{Role: "system", Content: "base prompt"},
		{Role: "user", Content: "hi"},
	}
	got := loop.withToolChainingHint(msgs)

	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if !strings.HasPrefix(got[0].Content, "base prompt") {
		t.Fatalf("expected original prompt preserved, got %q", got[0].Content)
	}
	if !strings.Contains(got[0].Content, toolChainingSentinel) {
		t.Fatalf("expected hint merged into system message, got %q", got[0].Content)
	}
	// Original slice must not be mutated.
	if msgs[0].Content != "base prompt" {
		t.Fatalf("input slice was mutated: %q", msgs[0].Content)
	}
}

func TestWithToolChainingHint_PrependsWhenNoSystemMessage(t *testing.T) {
	loop := newLoopWithTools(2)
	msgs := []history.Message{{Role: "user", Content: "hi"}}
	got := loop.withToolChainingHint(msgs)

	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if got[0].Role != "system" || !strings.Contains(got[0].Content, toolChainingSentinel) {
		t.Fatalf("expected system hint at head, got %+v", got[0])
	}
	if got[1].Role != "user" {
		t.Fatalf("expected user message preserved at index 1, got %+v", got[1])
	}
}

func TestWithToolChainingHint_SkipsWhenAlreadyPresent(t *testing.T) {
	loop := newLoopWithTools(2)
	msgs := []history.Message{
		{Role: "system", Content: "I already document <output_of:ID.field> syntax."},
		{Role: "user", Content: "hi"},
	}
	got := loop.withToolChainingHint(msgs)

	if got[0].Content != msgs[0].Content {
		t.Fatalf("expected unchanged prompt when sentinel present, got %q", got[0].Content)
	}
}

func TestWithToolChainingHint_DisableFlag(t *testing.T) {
	loop := newLoopWithTools(2)
	loop.DisableToolChainingHint = true
	msgs := []history.Message{{Role: "system", Content: "base"}}
	got := loop.withToolChainingHint(msgs)
	if got[0].Content != "base" {
		t.Fatalf("expected no injection when DisableToolChainingHint=true, got %q", got[0].Content)
	}
}

// systemCapturingProvider records the last system message content it received
// on each call, so tests can assert whether the hint reached the LLM request
// without touching the persisted session.
type systemCapturingProvider struct {
	mu     sync.Mutex
	seen   []string
	toolCh []PendingToolCall
	reply  string
}

func (p *systemCapturingProvider) GenerateStream(_ context.Context, memory []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.mu.Lock()
	for _, m := range memory {
		if m.Role == "system" {
			p.seen = append(p.seen, m.Content)
			break
		}
	}
	toolCalls := p.toolCh
	p.toolCh = nil
	p.mu.Unlock()

	if len(toolCalls) == 0 {
		ch <- Event(ContentEvent{Text: p.reply})
		return LLMResult{Content: p.reply}, nil
	}
	return LLMResult{ToolCalls: toolCalls}, nil
}

func (p *systemCapturingProvider) lastSystem() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) == 0 {
		return ""
	}
	return p.seen[len(p.seen)-1]
}

func TestAgentLoop_HintReachesLLM_ButNotPersistedSession(t *testing.T) {
	provider := &systemCapturingProvider{reply: "ok"}
	sm := history.NewInMemSessionManager("base system prompt")
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "a"})
	reg.Register(&echoTool{name: "b"})
	loop := NewAgentLoop(sm, reg, provider)

	if _, err := loop.RunIteration(context.Background(), "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}

	// LLM saw the hint merged into the system prompt.
	got := provider.lastSystem()
	if !strings.Contains(got, "base system prompt") {
		t.Fatalf("expected LLM to see base prompt, got %q", got)
	}
	if !strings.Contains(got, toolChainingSentinel) {
		t.Fatalf("expected LLM to see chaining hint, got %q", got)
	}

	// Persisted session does NOT contain the hint — only the original prompt
	// plus the user turn and assistant reply.
	stored, _ := sm.History(context.Background(), "s1")
	for _, m := range stored {
		if m.Role == "system" && strings.Contains(m.Content, toolChainingSentinel) {
			t.Fatalf("persisted session leaked the hint: %q", m.Content)
		}
	}
}
