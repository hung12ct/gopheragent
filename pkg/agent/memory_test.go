package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	loop := New(sm, reg, capture, WithMemory(store, MemoryConfig{}))

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
	loop := New(sm, reg, capture, WithMemory(store, MemoryConfig{}))

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
	res, err := c.Consolidate(context.Background(), "u", transcript)
	if err != nil {
		t.Fatalf("unexpected error on short transcript: %v", err)
	}
	if res.After != 0 {
		t.Fatalf("expected 0 notes after short transcript, got %d", res.After)
	}
}

func TestConsolidate_WritesNotesFromLLM(t *testing.T) {
	store := memory.NewInMemStore()
	prov := &consolidatorJSONProvider{json: `{"notes":[{"key":"pref","content":"user likes Go"},{"key":"db","content":"users table uses created_dt"}]}`}
	c := &Consolidator{Store: store, LLM: prov, MinTranscriptMessages: 1}

	transcript := []history.Message{
		{Role: "user", Content: "let's discuss"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "more"},
		{Role: "assistant", Content: "sure"},
	}
	res, err := c.Consolidate(context.Background(), "u", transcript)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.After != 2 {
		t.Fatalf("expected 2 notes after consolidation, got %d", res.After)
	}
	notes, _ := store.List(context.Background(), "u", memory.ListOpts{})
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes in store, got %d", len(notes))
	}
}

func TestConsolidate_MergesExistingWithNew(t *testing.T) {
	store := memory.NewInMemStore()
	ctx := context.Background()
	// Seed two existing notes; one will be kept-as-is, one updated by the merge.
	_ = store.Put(ctx, "u", memory.Note{Key: "keep", Content: "user is in UTC+7"})
	_ = store.Put(ctx, "u", memory.Note{Key: "outdated", Content: "user prefers Python"})

	// LLM "merges": drops 'outdated', keeps 'keep' with same key, adds 'new'.
	prov := &consolidatorJSONProvider{json: `{"notes":[
		{"key":"keep","content":"user is in UTC+7"},
		{"key":"new","content":"user now prefers Go"}
	]}`}
	c := &Consolidator{Store: store, LLM: prov, MinTranscriptMessages: 1}

	transcript := []history.Message{
		{Role: "user", Content: "from now on use Go not Python"},
		{Role: "assistant", Content: "noted"},
		{Role: "user", Content: "thanks"},
	}
	res, err := c.Consolidate(ctx, "u", transcript)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.Before != 2 || res.After != 2 {
		t.Fatalf("expected Before=2 After=2, got %+v", res)
	}
	notes, _ := store.List(ctx, "u", memory.ListOpts{})
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes after merge, got %d", len(notes))
	}
	got := map[string]string{}
	for _, n := range notes {
		got[n.Key] = n.Content
	}
	if _, exists := got["outdated"]; exists {
		t.Fatal("expected 'outdated' to be dropped by merge")
	}
	if got["new"] != "user now prefers Go" {
		t.Fatalf("expected 'new' present, got %v", got)
	}
}

func TestConsolidate_CapsAtMaxNotes(t *testing.T) {
	store := memory.NewInMemStore()
	// LLM returns 5; consolidator MaxNotes=3 → only 3 land.
	prov := &consolidatorJSONProvider{json: `{"notes":[
		{"key":"a","content":"1"},
		{"key":"b","content":"2"},
		{"key":"c","content":"3"},
		{"key":"d","content":"4"},
		{"key":"e","content":"5"}
	]}`}
	c := &Consolidator{Store: store, LLM: prov, MaxNotes: 3, MinTranscriptMessages: 1}

	transcript := []history.Message{
		{Role: "user", Content: "x"}, {Role: "assistant", Content: "y"}, {Role: "user", Content: "z"},
	}
	res, err := c.Consolidate(context.Background(), "u", transcript)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.After != 3 {
		t.Fatalf("expected After=3 after cap, got %d", res.After)
	}
}

func TestConsolidate_DedupesDuplicateKeysFromLLM(t *testing.T) {
	store := memory.NewInMemStore()
	prov := &consolidatorJSONProvider{json: `{"notes":[
		{"key":"dup","content":"first"},
		{"key":"dup","content":"second-version"},
		{"key":"other","content":"x"}
	]}`}
	c := &Consolidator{Store: store, LLM: prov, MinTranscriptMessages: 1}
	transcript := []history.Message{
		{Role: "user", Content: "x"}, {Role: "assistant", Content: "y"}, {Role: "user", Content: "z"},
	}
	res, err := c.Consolidate(context.Background(), "u", transcript)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.After != 2 {
		t.Fatalf("expected duplicate key collapsed to 1: After=%d", res.After)
	}
	notes, _ := store.List(context.Background(), "u", memory.ListOpts{})
	for _, n := range notes {
		if n.Key == "dup" && n.Content != "first" {
			t.Fatalf("expected first occurrence to win, got %q", n.Content)
		}
	}
}

func TestTrimToTokenBudget_HonorsBudget(t *testing.T) {
	notes := []memory.Note{
		{Content: strings.Repeat("a", 80)}, // ~20 tokens
		{Content: strings.Repeat("b", 80)},
		{Content: strings.Repeat("c", 80)},
		{Content: strings.Repeat("d", 80)},
	}
	// Header is ~36 tokens; each bullet (3 + 80 chars) is ~21 tokens.
	// Budget 70 tokens leaves ~34 tokens for bullets → fits one (~21t),
	// drops the rest.
	out := trimToTokenBudget(notes, 70)
	if len(out) != 1 {
		t.Fatalf("expected trim to 1 note at budget=70, got %d", len(out))
	}
	// Zero budget returns empty.
	if got := trimToTokenBudget(notes, 0); len(got) != len(notes) {
		t.Fatalf("zero budget should be a no-op (current behaviour), got %d", len(got))
	}
	// Header doesn't fit → nothing returned.
	if got := trimToTokenBudget(notes, 5); len(got) != 0 {
		t.Fatalf("undersized budget should return nothing, got %d", len(got))
	}
}

func TestLoadMemoryNotes_AppliesTokenBudget(t *testing.T) {
	store := memory.NewInMemStore()
	ctx := context.Background()
	// 20 notes, each ~120 chars → ~30 tokens each.
	for i := range 20 {
		_ = store.Put(ctx, "u", memory.Note{
			Key:     "k-" + strconv.Itoa(i),
			Content: strings.Repeat("x", 120),
		})
	}
	al := &AgentLoop{Memory: store, MemoryCfg: MemoryConfig{TokenBudget: 200}}
	got := al.loadMemoryNotes(ctx, "u")
	// Block should be capped: ~120 tokens budget after the header, fits ~4 notes.
	bullets := strings.Count(got, "\n- ")
	if bullets > 6 {
		t.Fatalf("budget should trim aggressively, got %d bullets", bullets)
	}
	if bullets == 0 {
		t.Fatalf("expected some bullets to fit in 200-token budget, got 0; output=%q", got)
	}
}

func TestLoadMemoryNotes_AppliesMaxNotes(t *testing.T) {
	store := memory.NewInMemStore()
	ctx := context.Background()
	for i := range 20 {
		_ = store.Put(ctx, "u", memory.Note{Key: "k-" + strconv.Itoa(i), Content: "x"})
	}
	al := &AgentLoop{Memory: store, MemoryCfg: MemoryConfig{MaxNotes: 5, TokenBudget: 10000}}
	got := al.loadMemoryNotes(ctx, "u")
	if bullets := strings.Count(got, "\n- "); bullets != 5 {
		t.Fatalf("expected MaxNotes=5 bullets, got %d in %q", bullets, got)
	}
}

func TestLoadMemoryForRun_FailClosedOnEmptyScope(t *testing.T) {
	store := memory.NewInMemStore()
	ctx := context.Background()
	// Seed a note under the "anonymous" scope to prove fail-closed
	// doesn't accidentally fall back to it.
	_ = store.Put(ctx, "", memory.Note{Key: "k", Content: "should not appear"})
	_ = store.Put(ctx, "anonymous", memory.Note{Key: "k", Content: "should not appear"})

	al := &AgentLoop{
		Memory: store,
		MemoryScopeFn: func(_ context.Context, _ string) string {
			return "" // simulate unauthenticated request
		},
	}
	scope, block, count := al.loadMemoryForRun(ctx, "s1")
	if scope != "" || block != "" || count != 0 {
		t.Fatalf("fail-closed broken: scope=%q block=%q count=%d", scope, block, count)
	}
}

func TestRunIteration_FailClosedSkipsMemoryAndEvent(t *testing.T) {
	store := memory.NewInMemStore()
	ctx := context.Background()
	_ = store.Put(ctx, "user:alice", memory.Note{Key: "k", Content: "alice secret"})

	capture := &systemPromptCapturingProvider{}
	sm := history.NewInMemSessionManager("base")
	reg := tools.NewRegistry()
	var loadedEvents []MemoryLoadedEvent
	loop := New(sm, reg, capture,
		WithMemory(store, MemoryConfig{}),
		WithMemoryScope(func(_ context.Context, _ string) string { return "" }),
		WithOnEvent(func(_ context.Context, _ string, ev StreamEvent) {
			if p, ok := ev.Payload.(MemoryLoadedEvent); ok {
				loadedEvents = append(loadedEvents, p)
			}
		}),
	)
	if _, err := loop.RunIteration(ctx, "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if strings.Contains(capture.firstSystem, "alice secret") {
		t.Fatalf("fail-closed must not leak notes from any scope: %q", capture.firstSystem)
	}
	if len(loadedEvents) != 0 {
		t.Fatalf("expected no MemoryLoadedEvent on fail-closed Run, got %d: %+v", len(loadedEvents), loadedEvents)
	}
}

func TestRunIteration_EmitsMemoryLoadedEvent(t *testing.T) {
	store := memory.NewInMemStore()
	ctx := context.Background()
	_ = store.Put(ctx, "user:alice", memory.Note{Key: "k1", Content: "fact one"})
	_ = store.Put(ctx, "user:alice", memory.Note{Key: "k2", Content: "fact two"})

	capture := &systemPromptCapturingProvider{}
	sm := history.NewInMemSessionManager("base")
	reg := tools.NewRegistry()
	var got []MemoryLoadedEvent
	loop := New(sm, reg, capture,
		WithMemory(store, MemoryConfig{}),
		WithMemoryScope(func(_ context.Context, _ string) string { return "user:alice" }),
		WithOnEvent(func(_ context.Context, _ string, ev StreamEvent) {
			if p, ok := ev.Payload.(MemoryLoadedEvent); ok {
				got = append(got, p)
			}
		}),
	)
	if _, err := loop.RunIteration(ctx, "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 MemoryLoadedEvent, got %d", len(got))
	}
	if got[0].Scope != "user:alice" || got[0].NoteCount != 2 {
		t.Fatalf("event payload wrong: %+v", got[0])
	}
	if got[0].EstimatedTokens <= 0 {
		t.Fatalf("expected positive token estimate, got %d", got[0].EstimatedTokens)
	}
}

func TestRunIteration_EmitsMemoryLoadedOnStoreError(t *testing.T) {
	// Even when the store errors, the audit event must fire so
	// adopters can detect the attempt with NoteCount=0.
	capture := &systemPromptCapturingProvider{}
	sm := history.NewInMemSessionManager("base")
	reg := tools.NewRegistry()
	var got []MemoryLoadedEvent
	loop := New(sm, reg, capture,
		WithMemory(&erroringStore{}, MemoryConfig{}),
		WithMemoryScope(func(_ context.Context, _ string) string { return "user:alice" }),
		WithOnEvent(func(_ context.Context, _ string, ev StreamEvent) {
			if p, ok := ev.Payload.(MemoryLoadedEvent); ok {
				got = append(got, p)
			}
		}),
	)
	if _, err := loop.RunIteration(context.Background(), "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if len(got) != 1 || got[0].Scope != "user:alice" || got[0].NoteCount != 0 {
		t.Fatalf("expected one event with NoteCount=0 on store error, got %+v", got)
	}
}

func TestFireConsolidator_SkipsOnEmptyScope(t *testing.T) {
	store := memory.NewInMemStore()
	prov := &consolidatorPanicProvider{} // panics if called
	al := &AgentLoop{
		Sessions:           history.NewInMemSessionManager("sys"),
		Memory:             store,
		MemoryScopeFn:      func(_ context.Context, _ string) string { return "" },
		MemoryConsolidator: &Consolidator{Store: store, LLM: prov},
	}
	// Should not panic, should not call provider, should not fire any event.
	al.fireConsolidator(context.Background(), "s1")
}

func TestShouldFire_FirstFireAlwaysAllowed(t *testing.T) {
	c := &Consolidator{FirePolicy: FirePolicy{MinInterval: time.Hour, NTurns: 100}}
	if !c.shouldFire("user:alice") {
		t.Fatal("first ever fire for a scope must be allowed regardless of policy")
	}
}

func TestShouldFire_DisabledNeverFires(t *testing.T) {
	c := &Consolidator{FirePolicy: FirePolicy{Disabled: true}}
	for range 5 {
		if c.shouldFire("user:alice") {
			t.Fatal("Disabled policy must never allow a fire")
		}
	}
}

func TestShouldFire_MinIntervalBlocksRapidFires(t *testing.T) {
	c := &Consolidator{FirePolicy: FirePolicy{MinInterval: time.Hour}}
	if !c.shouldFire("user:alice") {
		t.Fatal("first fire blocked")
	}
	for range 5 {
		if c.shouldFire("user:alice") {
			t.Fatal("MinInterval=1h must block rapid follow-up fires")
		}
	}
}

func TestShouldFire_NTurnsBlocksUntilThreshold(t *testing.T) {
	// No MinInterval so the test exercises NTurns only.
	c := &Consolidator{FirePolicy: FirePolicy{NTurns: 3}}
	if !c.shouldFire("u") {
		t.Fatal("first fire blocked")
	}
	// Two follow-up Runs blocked, third allowed.
	for i := range 2 {
		if c.shouldFire("u") {
			t.Fatalf("Run %d should be blocked by NTurns=3", i+2)
		}
	}
	if !c.shouldFire("u") {
		t.Fatal("Run 4 should be allowed: NTurns threshold met")
	}
}

func TestShouldFire_PerScopeIsolation(t *testing.T) {
	c := &Consolidator{FirePolicy: FirePolicy{MinInterval: time.Hour}}
	if !c.shouldFire("alice") {
		t.Fatal("alice first fire blocked")
	}
	// bob's first fire must succeed even though alice just fired —
	// the throttle is per-scope.
	if !c.shouldFire("bob") {
		t.Fatal("bob first fire blocked by alice's throttle (per-scope isolation broken)")
	}
}

func TestShouldFire_DefaultPolicyApplied(t *testing.T) {
	// Zero-value FirePolicy must resolve to DefaultFirePolicy
	// (MinInterval: 10m). First fire allowed; second within the
	// interval blocked.
	c := &Consolidator{}
	if !c.shouldFire("u") {
		t.Fatal("first fire blocked under default policy")
	}
	if c.shouldFire("u") {
		t.Fatal("second fire within 10m must be blocked under default policy")
	}
}

func TestShouldFire_BothThrottlesMustAllow(t *testing.T) {
	// MinInterval expired but NTurns not yet → blocked.
	c := &Consolidator{FirePolicy: FirePolicy{
		MinInterval: time.Nanosecond, // effectively always satisfied after first
		NTurns:      10,
	}}
	if !c.shouldFire("u") {
		t.Fatal("first fire blocked")
	}
	// MinInterval is 1ns so time satisfies; NTurns requires 10 turns.
	if c.shouldFire("u") {
		t.Fatal("AND semantic broken: NTurns should still block even though MinInterval passed")
	}
}

func TestFireConsolidator_HonorsFirePolicy(t *testing.T) {
	// Real Consolidate path under Run, with FirePolicy{NTurns: 3}.
	// Three Runs: first fires, next two are blocked.
	store := memory.NewInMemStore()
	prov := &consolidatorJSONProvider{json: `{"notes":[{"key":"k","content":"v"}]}`}
	c := &Consolidator{
		Store:                 store,
		LLM:                   prov,
		MinTranscriptMessages: 1,
		FirePolicy:            FirePolicy{NTurns: 3},
	}
	sm := history.NewInMemSessionManager("base")
	reg := tools.NewRegistry()
	var consolidated []MemoryConsolidatedEvent
	var mu sync.Mutex
	loop := New(sm, reg, &systemPromptCapturingProvider{},
		WithMemory(store, MemoryConfig{}),
		WithMemoryScope(func(_ context.Context, _ string) string { return "user:alice" }),
		WithMemoryConsolidator(c),
		WithOnEvent(func(_ context.Context, _ string, ev StreamEvent) {
			if p, ok := ev.Payload.(MemoryConsolidatedEvent); ok {
				mu.Lock()
				consolidated = append(consolidated, p)
				mu.Unlock()
			}
		}),
	)
	for i := range 3 {
		if _, err := loop.RunIteration(context.Background(), "s1", "turn "+strconv.Itoa(i)); err != nil {
			t.Fatalf("RunIteration %d: %v", i, err)
		}
	}
	// Wait briefly for the detached consolidator goroutine to emit.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(consolidated)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(consolidated) != 1 {
		t.Fatalf("expected exactly 1 consolidation across 3 Runs under NTurns=3, got %d", len(consolidated))
	}
}

func TestShutdown_NoopWhenNoBackgroundWork(t *testing.T) {
	al := &AgentLoop{Sessions: history.NewInMemSessionManager("sys")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := al.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with no bg work should be instant, got %v", err)
	}
}

func TestShutdown_WaitsForInflightConsolidator(t *testing.T) {
	store := memory.NewInMemStore()
	// Provider holds the consolidator goroutine for 100ms.
	slowProv := &slowJSONProvider{json: `{"notes":[{"key":"k","content":"v"}]}`, delay: 100 * time.Millisecond}
	c := &Consolidator{Store: store, LLM: slowProv, MinTranscriptMessages: 1, FirePolicy: FirePolicy{NTurns: 1}}

	sm := history.NewInMemSessionManager("base")
	reg := tools.NewRegistry()
	loop := New(sm, reg, &systemPromptCapturingProvider{},
		WithMemory(store, MemoryConfig{}),
		WithMemoryScope(func(_ context.Context, _ string) string { return "u" }),
		WithMemoryConsolidator(c),
	)
	if _, err := loop.RunIteration(context.Background(), "s", "turn"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	// Shutdown must wait at least the goroutine's delay.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := loop.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 90*time.Millisecond {
		t.Fatalf("Shutdown returned in %v — should have waited ~100ms for consolidator", elapsed)
	}
	// Confirm consolidation actually completed.
	notes, _ := store.List(context.Background(), "u", memory.ListOpts{})
	if len(notes) != 1 {
		t.Fatalf("consolidation should have completed before Shutdown returned, got %d notes", len(notes))
	}
}

func TestShutdown_HonorsContextDeadline(t *testing.T) {
	store := memory.NewInMemStore()
	slowProv := &slowJSONProvider{json: `{"notes":[]}`, delay: 500 * time.Millisecond}
	c := &Consolidator{Store: store, LLM: slowProv, MinTranscriptMessages: 1, FirePolicy: FirePolicy{NTurns: 1}}

	sm := history.NewInMemSessionManager("base")
	reg := tools.NewRegistry()
	loop := New(sm, reg, &systemPromptCapturingProvider{},
		WithMemory(store, MemoryConfig{}),
		WithMemoryScope(func(_ context.Context, _ string) string { return "u" }),
		WithMemoryConsolidator(c),
	)
	if _, err := loop.RunIteration(context.Background(), "s", "turn"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	// Deadline shorter than the consolidator delay.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := loop.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown should have returned ctx.DeadlineExceeded")
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

// erroringStore returns an error on every List/Put/ReplaceAll/Delete
// — used to assert the audit event still fires even when the store
// path fails.
type erroringStore struct{}

func (erroringStore) Put(_ context.Context, _ string, _ memory.Note) error {
	return errStoreSynthetic
}
func (erroringStore) List(_ context.Context, _ string, _ memory.ListOpts) ([]memory.Note, error) {
	return nil, errStoreSynthetic
}
func (erroringStore) Delete(_ context.Context, _, _ string) error { return errStoreSynthetic }
func (erroringStore) ReplaceAll(_ context.Context, _ string, _ []memory.Note) error {
	return errStoreSynthetic
}

var errStoreSynthetic = errors.New("memory: synthetic store failure")

// slowJSONProvider sleeps before returning a fixed JSON body — used
// to give Shutdown tests a measurable in-flight window to wait on.
type slowJSONProvider struct {
	json  string
	delay time.Duration
}

func (p *slowJSONProvider) GenerateStream(ctx context.Context, _ []history.Message, _ *tools.Registry, _ chan<- StreamEvent) (LLMResult, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return LLMResult{}, ctx.Err()
	}
	return LLMResult{Content: p.json}, nil
}

// consolidatorPanicProvider panics if called — used to assert short
// transcripts never reach the provider.
type consolidatorPanicProvider struct{}

func (p *consolidatorPanicProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- StreamEvent) (LLMResult, error) {
	panic("provider should not be called for short transcripts")
}

