package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// cacheHintCapturingProvider records whether the first system message of
// every incoming message slice has CacheHint=true. One-shot: returns
// Content:"ok" on the first call.
type cacheHintCapturingProvider struct {
	mu         sync.Mutex
	seenStamps []bool // one entry per GenerateStream call
}

func (p *cacheHintCapturingProvider) GenerateStream(_ context.Context, msgs []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.mu.Lock()
	stamp := false
	if len(msgs) > 0 && msgs[0].Role == "system" && msgs[0].CacheHint {
		stamp = true
	}
	p.seenStamps = append(p.seenStamps, stamp)
	p.mu.Unlock()
	ch <- Event(ContentEvent{Text: "ok"})
	return LLMResult{Content: "ok"}, nil
}

func TestAutoCacheSystem_DefaultOnViaConstructor(t *testing.T) {
	provider := &cacheHintCapturingProvider{}
	sm := history.NewInMemSessionManager("base prompt")
	loop := NewAgentLoop(sm, tools.NewRegistry(), provider)

	if !loop.AutoCacheSystem {
		t.Fatal("NewAgentLoop should default AutoCacheSystem to true")
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "hello"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if len(provider.seenStamps) == 0 || !provider.seenStamps[0] {
		t.Fatalf("expected first system message to carry CacheHint=true, got %v", provider.seenStamps)
	}
}

func TestAutoCacheSystem_StructLiteralDefaultsOff(t *testing.T) {
	provider := &cacheHintCapturingProvider{}
	sm := history.NewInMemSessionManager("base prompt")
	// Intentional struct-literal construction — caller takes responsibility
	// for opting into AutoCacheSystem.
	loop := &AgentLoop{
		Sessions:     sm,
		Tools:        tools.NewRegistry(),
		LLM:          provider,
		MaxIters:     15,
		EmitThoughts: true,
	}
	if loop.AutoCacheSystem {
		t.Fatal("struct-literal construction should leave AutoCacheSystem=false (zero value)")
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "hello"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if len(provider.seenStamps) == 0 || provider.seenStamps[0] {
		t.Fatalf("AutoCacheSystem=false should not stamp CacheHint, got %v", provider.seenStamps)
	}
}

func TestAutoCacheSystem_DoesNotLeakIntoSessionHistory(t *testing.T) {
	provider := &cacheHintCapturingProvider{}
	sm := history.NewInMemSessionManager("base prompt")
	loop := NewAgentLoop(sm, tools.NewRegistry(), provider)

	if _, err := loop.RunIteration(context.Background(), "s1", "hello"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}

	stored, _ := sm.History(context.Background(), "s1")
	for i, m := range stored {
		if m.CacheHint {
			t.Fatalf("session history message %d (%s) carries CacheHint=true; the flag should be ephemeral to the LLM call",
				i, m.Role)
		}
	}
}
