package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func TestWithDynamicContext_NilIsZeroCost(t *testing.T) {
	al := &AgentLoop{}
	msgs := []history.Message{{Role: "system", Content: "base"}}
	out := al.withDynamicContext(context.Background(), "s1", msgs)
	if len(out) != 1 || out[0].Content != "base" {
		t.Fatalf("nil augmenter should return input unchanged, got %+v", out)
	}
}

func TestWithDynamicContext_AppendsToSystem(t *testing.T) {
	al := &AgentLoop{DynamicContext: func(_ context.Context, _ string) string { return "today is X" }}
	msgs := []history.Message{{Role: "system", Content: "base"}}
	out := al.withDynamicContext(context.Background(), "s1", msgs)
	if !strings.Contains(out[0].Content, "base") {
		t.Fatalf("base prompt dropped: %q", out[0].Content)
	}
	if !strings.Contains(out[0].Content, "today is X") {
		t.Fatalf("addition not appended: %q", out[0].Content)
	}
	if !strings.Contains(out[0].Content, dynamicContextSentinel) {
		t.Fatalf("sentinel missing: %q", out[0].Content)
	}
	// Input must not be mutated.
	if msgs[0].Content != "base" {
		t.Fatalf("input msgs mutated: %q", msgs[0].Content)
	}
}

func TestWithDynamicContext_SynthesizesSystem(t *testing.T) {
	al := &AgentLoop{DynamicContext: func(_ context.Context, _ string) string { return "ctx" }}
	msgs := []history.Message{{Role: "user", Content: "hi"}}
	out := al.withDynamicContext(context.Background(), "s1", msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages (synthesized system + user), got %d", len(out))
	}
	if out[0].Role != "system" {
		t.Fatalf("expected first message to be system, got %q", out[0].Role)
	}
	if !strings.Contains(out[0].Content, dynamicContextSentinel) || !strings.Contains(out[0].Content, "ctx") {
		t.Fatalf("synthesized system missing sentinel/addition: %q", out[0].Content)
	}
	if out[1].Role != "user" || out[1].Content != "hi" {
		t.Fatalf("user message not preserved: %+v", out[1])
	}
}

func TestWithDynamicContext_Idempotent(t *testing.T) {
	al := &AgentLoop{DynamicContext: func(_ context.Context, _ string) string { return "ctx" }}
	msgs := []history.Message{{Role: "system", Content: "base"}}
	once := al.withDynamicContext(context.Background(), "s1", msgs)
	twice := al.withDynamicContext(context.Background(), "s1", once)
	if strings.Count(twice[0].Content, dynamicContextSentinel) != 1 {
		t.Fatalf("sentinel appears %d times, want 1: %q",
			strings.Count(twice[0].Content, dynamicContextSentinel), twice[0].Content)
	}
}

func TestWithDynamicContext_EmptyReturnSkips(t *testing.T) {
	al := &AgentLoop{DynamicContext: func(_ context.Context, _ string) string { return "" }}
	msgs := []history.Message{{Role: "system", Content: "base"}}
	out := al.withDynamicContext(context.Background(), "s1", msgs)
	if len(out) != 1 || out[0].Content != "base" {
		t.Fatalf("empty return should skip, got %+v", out)
	}
}

func TestWithDynamicContext_PassesSessionKey(t *testing.T) {
	var gotKey string
	al := &AgentLoop{DynamicContext: func(_ context.Context, sessionKey string) string {
		gotKey = sessionKey
		return ""
	}}
	al.withDynamicContext(context.Background(), "session-42", []history.Message{{Role: "system", Content: "base"}})
	if gotKey != "session-42" {
		t.Fatalf("augmenter got sessionKey %q, want %q", gotKey, "session-42")
	}
}

func TestDynamicContext_NotPersisted(t *testing.T) {
	provider := &systemCapturingProviderPM{turns: []LLMResult{{Content: "ok"}}}
	sm := history.NewInMemSessionManager("base prompt")
	reg := tools.NewRegistry()
	loop := NewAgentLoop(sm, reg, provider)
	loop.DynamicContext = func(_ context.Context, _ string) string {
		return "ephemeral: today is Tuesday"
	}

	_, err := loop.RunIteration(context.Background(), "s1", "hello")
	if err != nil {
		t.Fatalf("RunIteration: %v", err)
	}

	// LLM must have seen the augmentation.
	if len(provider.capturedSP) == 0 {
		t.Fatal("provider received no system prompt")
	}
	if !strings.Contains(provider.capturedSP[0], "today is Tuesday") {
		t.Fatalf("LLM did not see the dynamic context: %q", provider.capturedSP[0])
	}
	if !strings.Contains(provider.capturedSP[0], dynamicContextSentinel) {
		t.Fatalf("LLM-bound system prompt missing sentinel: %q", provider.capturedSP[0])
	}

	// Session history must NOT contain the augmentation — it's ephemeral.
	stored := sm.GetHistory(context.Background(), "s1")
	for _, m := range stored {
		if strings.Contains(m.Content, dynamicContextSentinel) {
			t.Fatalf("dynamic context leaked into session history: %+v", m)
		}
		if strings.Contains(m.Content, "ephemeral: today is Tuesday") {
			t.Fatalf("dynamic context text leaked into session history: %+v", m)
		}
	}
}
