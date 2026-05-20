package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/memory"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func TestWithMemoryNotesInSystem_AppendsToSystem(t *testing.T) {
	al := &AgentLoop{}
	ctx := withMemoryNotes(context.Background(), "\n\n## Long-term memory\n- note one\n")
	msgs := []history.Message{{Role: "system", Content: "base prompt"}}
	out := al.withMemoryNotesInSystem(ctx, msgs)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	if !strings.HasPrefix(out[0].Content, "base prompt") {
		t.Fatalf("base prompt should remain at the start: %q", out[0].Content)
	}
	if !strings.Contains(out[0].Content, "- note one") {
		t.Fatalf("memory block should be appended: %q", out[0].Content)
	}
	if msgs[0].Content != "base prompt" {
		t.Fatalf("caller slice should not be mutated, got %q", msgs[0].Content)
	}
}

func TestWithMemoryNotesInSystem_IdempotentAcrossRetries(t *testing.T) {
	al := &AgentLoop{}
	ctx := withMemoryNotes(context.Background(), "block")
	msgs := []history.Message{{Role: "system", Content: "base"}}
	once := al.withMemoryNotesInSystem(ctx, msgs)
	twice := al.withMemoryNotesInSystem(ctx, once)
	if once[0].Content != twice[0].Content {
		t.Fatalf("second injection should be a no-op: once=%q twice=%q", once[0].Content, twice[0].Content)
	}
}

func TestWithMemoryNotesInSystem_SynthesizesSystemWhenAbsent(t *testing.T) {
	al := &AgentLoop{}
	ctx := withMemoryNotes(context.Background(), "\n## Long-term memory\n- only note\n")
	out := al.withMemoryNotesInSystem(ctx, nil)
	if len(out) != 1 || out[0].Role != "system" {
		t.Fatalf("expected synthesized system message, got %+v", out)
	}
	if !strings.Contains(out[0].Content, "- only note") {
		t.Fatalf("note missing: %q", out[0].Content)
	}
}

func TestWithMemoryNotesInSystem_EmptyNotesIsNoop(t *testing.T) {
	al := &AgentLoop{}
	msgs := []history.Message{{Role: "system", Content: "base"}}
	out := al.withMemoryNotesInSystem(context.Background(), msgs)
	if len(out) != 1 || out[0].Content != "base" {
		t.Fatalf("empty ctx should be a no-op, got %+v", out)
	}
}

func TestLoadMemoryNotes_DisabledWhenStoreNil(t *testing.T) {
	al := &AgentLoop{}
	if got := al.loadMemoryNotes(context.Background(), "s1"); got != "" {
		t.Fatalf("expected empty when Memory is nil, got %q", got)
	}
}

func TestLoadMemoryNotes_UsesScopeFn(t *testing.T) {
	store := memory.NewInMemStore()
	ctx := context.Background()
	_ = store.Put(ctx, "user:alice", memory.Note{Key: "k", Content: "alice fact"})

	al := &AgentLoop{
		Memory: store,
		MemoryScopeFn: func(_ context.Context, sessionKey string) string {
			if sessionKey == "alice-session" {
				return "user:alice"
			}
			return sessionKey
		},
	}
	out := al.loadMemoryNotes(ctx, "alice-session")
	if !strings.Contains(out, "alice fact") {
		t.Fatalf("expected scope-resolved notes, got %q", out)
	}
	// A session that doesn't map to a known scope returns no notes.
	if got := al.loadMemoryNotes(ctx, "unknown"); got != "" {
		t.Fatalf("expected empty for unknown scope, got %q", got)
	}
}

func TestRunIteration_InjectsMemoryNotesOnFreshSession(t *testing.T) {
	store := memory.NewInMemStore()
	ctx := context.Background()
	_ = store.Put(ctx, "s1", memory.Note{Key: "pref", Content: "user prefers metric units"})

	capture := &systemPromptCapturingProvider{}
	sm := history.NewInMemSessionManager("base system")
	reg := tools.NewRegistry()
	loop := New(sm, reg, capture, WithMemory(store))

	if _, err := loop.RunIteration(ctx, "s1", "hello"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if !strings.Contains(capture.firstSystem, "user prefers metric units") {
		t.Fatalf("memory notes missing from system prompt: %q", capture.firstSystem)
	}
	if !strings.Contains(capture.firstSystem, "base system") {
		t.Fatalf("base system prompt lost: %q", capture.firstSystem)
	}
}

func TestRunIteration_InjectsLatestNotesOnEveryTurn(t *testing.T) {
	store := memory.NewInMemStore()
	ctx := context.Background()
	_ = store.Put(ctx, "s1", memory.Note{Key: "pref", Content: "user prefers metric units"})

	capture := &systemPromptCapturingProvider{}
	sm := history.NewInMemSessionManager("base system")
	reg := tools.NewRegistry()
	loop := New(sm, reg, capture, WithMemory(store))

	// Turn 1: notes from initial store state inject.
	if _, err := loop.RunIteration(ctx, "s1", "first"); err != nil {
		t.Fatalf("RunIteration #1: %v", err)
	}
	if !strings.Contains(capture.firstSystem, "user prefers metric units") {
		t.Fatalf("turn 1 missing initial note: %q", capture.firstSystem)
	}

	// Add a new note between turns. Turn 2 must reflect the updated
	// store snapshot — adopters expect freshly-consolidated knowledge
	// to apply on the next turn within the same session.
	_ = store.Put(ctx, "s1", memory.Note{Key: "later", Content: "added between turns"})

	capture.reset()
	if _, err := loop.RunIteration(ctx, "s1", "second"); err != nil {
		t.Fatalf("RunIteration #2: %v", err)
	}
	if !strings.Contains(capture.firstSystem, "added between turns") {
		t.Fatalf("turn 2 should see new note: %q", capture.firstSystem)
	}
	if !strings.Contains(capture.firstSystem, "user prefers metric units") {
		t.Fatalf("turn 2 should still see original note: %q", capture.firstSystem)
	}
}

func TestConsolidate_SkipsShortTranscripts(t *testing.T) {
	c := &Consolidator{Store: memory.NewInMemStore(), LLM: &consolidatorPanicProvider{}}
	transcript := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
	}
	n, err := c.Consolidate(context.Background(), "u", transcript)
	if err != nil {
		t.Fatalf("unexpected error on short transcript: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 notes from short transcript, got %d", n)
	}
}

func TestConsolidate_WritesNotesFromLLM(t *testing.T) {
	store := memory.NewInMemStore()
	prov := &consolidatorJSONProvider{json: `{"notes":[{"key":"pref","content":"user likes Go"},{"key":"db","content":"users table uses created_dt"}]}`}
	c := &Consolidator{Store: store, LLM: prov}

	transcript := []history.Message{
		{Role: "user", Content: "let's discuss"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "more"},
		{Role: "assistant", Content: "sure"},
	}
	n, err := c.Consolidate(context.Background(), "u", transcript)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 notes written, got %d", n)
	}
	notes, _ := store.List(context.Background(), "u")
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes in store, got %d", len(notes))
	}
}

func TestFireConsolidator_NoopWhenNil(t *testing.T) {
	al := &AgentLoop{Sessions: history.NewInMemSessionManager("sys")}
	// Should not panic and should return immediately.
	al.fireConsolidator(context.Background(), "s1")
}

// --- helpers ---

type systemPromptCapturingProvider struct {
	mu          sync.Mutex
	firstSystem string
}

func (p *systemPromptCapturingProvider) GenerateStream(_ context.Context, msgs []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.mu.Lock()
	if p.firstSystem == "" {
		for _, m := range msgs {
			if m.Role == "system" {
				p.firstSystem = m.Content
				break
			}
		}
	}
	p.mu.Unlock()
	ch <- Event(ContentEvent{Text: "ok"})
	return LLMResult{Content: "ok"}, nil
}

func (p *systemPromptCapturingProvider) reset() {
	p.mu.Lock()
	p.firstSystem = ""
	p.mu.Unlock()
}

// consolidatorJSONProvider returns a fixed JSON body so the consolidator
// can be tested without a real LLM.
type consolidatorJSONProvider struct {
	json string
}

func (p *consolidatorJSONProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- StreamEvent) (LLMResult, error) {
	return LLMResult{Content: p.json}, nil
}

// consolidatorPanicProvider panics if called — used to assert short
// transcripts never reach the provider.
type consolidatorPanicProvider struct{}

func (p *consolidatorPanicProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- StreamEvent) (LLMResult, error) {
	panic("provider should not be called for short transcripts")
}

