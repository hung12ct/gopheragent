package llm

import (
	"context"
	"fmt"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// stubProvider records which provider was called and returns a fixed response.
type stubProvider struct {
	name   string
	called *string
}

func (p *stubProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- agent.StreamEvent) (agent.LLMResult, error) {
	*p.called = p.name
	ch <- agent.Event(agent.ContentEvent{Text: p.name + "_response"})
	return agent.LLMResult{Content: p.name + "_response"}, nil
}

func newStub(name string, called *string) *stubProvider {
	return &stubProvider{name: name, called: called}
}

func msgs(contents ...string) []history.Message {
	var out []history.Message
	for i, c := range contents {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, history.Message{Role: role, Content: c})
	}
	return out
}

func TestRouterProvider_UsesFirstMatchingRoute(t *testing.T) {
	var called string
	fast := newStub("fast", &called)
	powerful := newStub("powerful", &called)

	router := NewRouterProvider(powerful).
		AddRoute(IfTokensUnder(1000), fast)

	short := msgs("hi") // very short → IfTokensUnder(1000) matches
	ch := make(chan agent.StreamEvent, 10)
	router.GenerateStream(context.Background(), short, nil, ch)

	if called != "fast" {
		t.Fatalf("expected 'fast' provider, got %q", called)
	}
}

func TestRouterProvider_FallsBackToDefault(t *testing.T) {
	var called string
	fast := newStub("fast", &called)
	powerful := newStub("powerful", &called)

	router := NewRouterProvider(powerful).
		AddRoute(IfTokensUnder(0), fast) // threshold 0 → never matches (tokens >= 0)

	ch := make(chan agent.StreamEvent, 10)
	router.GenerateStream(context.Background(), msgs("hi"), nil, ch)

	if called != "powerful" {
		t.Fatalf("expected fallback 'powerful', got %q", called)
	}
}

func TestRouterProvider_MultipleRoutes_FirstWins(t *testing.T) {
	var called string
	r1 := newStub("r1", &called)
	r2 := newStub("r2", &called)
	fallback := newStub("fallback", &called)

	router := NewRouterProvider(fallback).
		AddRoute(Always(), r1).
		AddRoute(Always(), r2) // r1 should win

	ch := make(chan agent.StreamEvent, 10)
	router.GenerateStream(context.Background(), msgs("hi"), nil, ch)

	if called != "r1" {
		t.Fatalf("expected first matching route 'r1', got %q", called)
	}
}

func TestIfTokensUnder(t *testing.T) {
	short := msgs("hi")                               // ~1 token
	long := msgs(fmt.Sprintf("%s", make([]byte, 400))) // ~100 tokens

	if !IfTokensUnder(50)(short) {
		t.Fatal("short message should be under 50 tokens")
	}
	if IfTokensUnder(50)(long) {
		t.Fatal("long message should NOT be under 50 tokens")
	}
}

func TestIfMessageCountUnder(t *testing.T) {
	cond := IfMessageCountUnder(3)
	if !cond(msgs("a", "b")) {
		t.Fatal("2 messages should be under 3")
	}
	if cond(msgs("a", "b", "c")) {
		t.Fatal("3 messages should NOT be under 3")
	}
}

func TestIfLastMessageContains(t *testing.T) {
	cond := IfLastMessageContains("summarize", "tldr")

	match := []history.Message{{Role: "user", Content: "please SUMMARIZE this article"}}
	if !cond(match) {
		t.Fatal("should match keyword 'summarize' (case-insensitive)")
	}

	noMatch := []history.Message{{Role: "user", Content: "analyze this deeply"}}
	if cond(noMatch) {
		t.Fatal("should not match unrelated message")
	}

	// Only the last user message is checked
	old := []history.Message{
		{Role: "user", Content: "tldr please"},    // old message
		{Role: "assistant", Content: "summary"},
		{Role: "user", Content: "now go deeper"},  // last user → no match
	}
	if cond(old) {
		t.Fatal("should only check last user message")
	}
}

func TestIfSystemPromptContains(t *testing.T) {
	cond := IfSystemPromptContains("summarizer")

	withSystem := []history.Message{
		{Role: "system", Content: "You are a fast summarizer for user profiles."},
		{Role: "user", Content: "hi"},
	}
	if !cond(withSystem) {
		t.Fatal("should match system prompt keyword")
	}

	withoutSystem := []history.Message{{Role: "user", Content: "hi"}}
	if cond(withoutSystem) {
		t.Fatal("should not match when no system message")
	}
}

func TestAlways(t *testing.T) {
	cond := Always()
	if !cond(nil) {
		t.Fatal("Always() should always return true")
	}
	if !cond(msgs("anything")) {
		t.Fatal("Always() should always return true")
	}
}
