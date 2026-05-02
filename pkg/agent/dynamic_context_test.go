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
	// Zero-cost implies the same slice header is returned when nil.
	if &out[0] != &msgs[0] {
		t.Fatal("nil path should not copy the slice")
	}
}

func TestWithDynamicContext_AppendsAtTail(t *testing.T) {
	al := &AgentLoop{DynamicContext: func(_ context.Context, _ string) string { return "today is X" }}
	msgs := []history.Message{
		{Role: "system", Content: "base"},
		{Role: "user", Content: "hello"},
	}
	out := al.withDynamicContext(context.Background(), "s1", msgs)

	if len(out) != 3 {
		t.Fatalf("expected 3 messages (2 input + 1 appended), got %d", len(out))
	}
	// Historical prefix must be byte-identical — this is the cache-stability guarantee.
	if out[0].Content != "base" || out[1].Content != "hello" {
		t.Fatalf("historical messages mutated: %+v", out[:2])
	}
	// Tail must carry the dynamic payload.
	tail := out[len(out)-1]
	if tail.Role != "user" {
		t.Fatalf("tail role = %q, want 'user'", tail.Role)
	}
	if !strings.Contains(tail.Content, "today is X") {
		t.Fatalf("tail missing addition: %q", tail.Content)
	}
	if !strings.Contains(tail.Content, dynamicContextSentinel) {
		t.Fatalf("tail missing sentinel: %q", tail.Content)
	}
	// Input must not be mutated.
	if len(msgs) != 2 || msgs[0].Content != "base" || msgs[1].Content != "hello" {
		t.Fatalf("input msgs mutated: %+v", msgs)
	}
}

func TestWithDynamicContext_AppendsWhenOnlySystemPresent(t *testing.T) {
	al := &AgentLoop{DynamicContext: func(_ context.Context, _ string) string { return "ctx" }}
	msgs := []history.Message{{Role: "system", Content: "base"}}
	out := al.withDynamicContext(context.Background(), "s1", msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages (system + appended), got %d", len(out))
	}
	if out[0].Content != "base" {
		t.Fatalf("system message mutated: %q", out[0].Content)
	}
	if out[1].Role != "user" || !strings.Contains(out[1].Content, dynamicContextSentinel) {
		t.Fatalf("appended message malformed: %+v", out[1])
	}
}

func TestWithDynamicContext_AppendsWhenInputEmpty(t *testing.T) {
	al := &AgentLoop{DynamicContext: func(_ context.Context, _ string) string { return "ctx" }}
	out := al.withDynamicContext(context.Background(), "s1", nil)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	if out[0].Role != "user" || !strings.Contains(out[0].Content, "ctx") {
		t.Fatalf("appended message malformed: %+v", out[0])
	}
}

func TestWithDynamicContext_Idempotent(t *testing.T) {
	al := &AgentLoop{DynamicContext: func(_ context.Context, _ string) string { return "ctx" }}
	msgs := []history.Message{{Role: "system", Content: "base"}}
	once := al.withDynamicContext(context.Background(), "s1", msgs)
	twice := al.withDynamicContext(context.Background(), "s1", once)
	if len(twice) != len(once) {
		t.Fatalf("second call added a message: once=%d twice=%d", len(once), len(twice))
	}
	total := 0
	for _, m := range twice {
		total += strings.Count(m.Content, dynamicContextSentinel)
	}
	if total != 1 {
		t.Fatalf("sentinel appears %d times across all messages, want 1", total)
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

func TestWithDynamicContext_CachePrefixStableAcrossRotation(t *testing.T) {
	// Cache-stability guarantee: rotating the dynamic text must NOT change any
	// historical message — only the appended tail differs. This is what keeps
	// Anthropic prompt-cache breakpoints on persisted history valid across
	// dynamic rotations.
	hist := []history.Message{
		{Role: "system", Content: "stable system"},
		{Role: "user", Content: "turn 1 question"},
		{Role: "assistant", Content: "turn 1 answer"},
		{Role: "user", Content: "turn 2 question"},
	}

	alA := &AgentLoop{DynamicContext: func(_ context.Context, _ string) string { return "dynamic A" }}
	alB := &AgentLoop{DynamicContext: func(_ context.Context, _ string) string { return "dynamic B" }}

	outA := alA.withDynamicContext(context.Background(), "s1", hist)
	outB := alB.withDynamicContext(context.Background(), "s1", hist)

	if len(outA) != len(outB) || len(outA) != len(hist)+1 {
		t.Fatalf("unexpected lengths: A=%d B=%d hist=%d", len(outA), len(outB), len(hist))
	}
	for i := 0; i < len(hist); i++ {
		if outA[i].Role != outB[i].Role || outA[i].Content != outB[i].Content {
			t.Fatalf("historical message %d diverged between rotations — cache would miss. A=%+v B=%+v",
				i, outA[i], outB[i])
		}
	}
	if outA[len(outA)-1].Content == outB[len(outB)-1].Content {
		t.Fatal("tail should differ across rotations")
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

	// LLM must have received the augmentation somewhere in the message stream.
	// The provider captures only the system message in capturedSP; verify
	// instead via the session history NOT containing the sentinel.
	stored := sm.GetHistory(context.Background(), "s1")
	for _, m := range stored {
		if strings.Contains(m.Content, dynamicContextSentinel) {
			t.Fatalf("dynamic context leaked into session history: %+v", m)
		}
		if strings.Contains(m.Content, "ephemeral: today is Tuesday") {
			t.Fatalf("dynamic context text leaked into session history: %+v", m)
		}
	}
	// And the system prompt saved in history must NOT include the dynamic text.
	if len(stored) > 0 && stored[0].Role == "system" && strings.Contains(stored[0].Content, "ephemeral") {
		t.Fatalf("system prompt in history contaminated: %q", stored[0].Content)
	}
}

func TestDynamicContextFuncFromContext_NilWhenAbsent(t *testing.T) {
	if fn := DynamicContextFuncFromContext(context.Background()); fn != nil {
		t.Fatalf("expected nil DynamicContextFunc on bare ctx, got %v", fn)
	}
}

func TestWithDynamicContextFunc_NilFuncIsZeroCost(t *testing.T) {
	// Installing a nil func must return ctx unchanged so the parent loop
	// pays nothing when DynamicContext is unset.
	parent := context.Background()
	out := WithDynamicContextFunc(parent, nil)
	if out != parent {
		t.Fatalf("expected nil-fn install to return original ctx, got derived")
	}
}

func TestWithDynamicContextFunc_RoundTrip(t *testing.T) {
	want := "today is 2026-05-02"
	fn := DynamicContextFunc(func(_ context.Context, _ string) string { return want })
	ctx := WithDynamicContextFunc(context.Background(), fn)

	got := DynamicContextFuncFromContext(ctx)
	if got == nil {
		t.Fatal("DynamicContextFuncFromContext returned nil after install")
	}
	if got(ctx, "session-x") != want {
		t.Fatalf("round-trip func returned wrong value")
	}
}
