package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// reflectProvider scripts turns by counting calls. Each turn's Content is
// streamed as a "content" event before returning so both the main loop and
// reflection rounds flow through the normal streaming path.
type reflectProvider struct {
	mu            sync.Mutex
	turns         []string
	idx           int
	capturedTools []bool // whether each call received a non-nil tool registry
	lastMsgLens   []int
	// critiqued records the assistant answer each critique round was
	// asked to review, so a test can prove a rejected round was rolled
	// back before the next round saw the conversation.
	critiqued []string
}

// lastAssistant returns the content of the final assistant message, or ""
// when there is none (the initial answer turn).
func lastAssistant(msgs []history.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return msgs[i].Content
		}
	}
	return ""
}

func (p *reflectProvider) GenerateStream(_ context.Context, msgs []history.Message, tl *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.mu.Lock()
	p.capturedTools = append(p.capturedTools, tl != nil)
	p.lastMsgLens = append(p.lastMsgLens, len(msgs))
	p.critiqued = append(p.critiqued, lastAssistant(msgs))
	var text string
	if p.idx < len(p.turns) {
		text = p.turns[p.idx]
		p.idx++
	} else {
		text = "fallthrough"
	}
	p.mu.Unlock()
	if text != "" {
		ch <- Event(ContentEvent{Text: text})
	}
	return LLMResult{Content: text}, nil
}

func TestReflect_DisabledByDefault(t *testing.T) {
	// With Reflect=0 no critique turns fire — provider is called exactly once.
	p := &reflectProvider{turns: []string{"first answer"}}
	sm := history.NewInMemSessionManager("sys")
	loop := NewAgentLoop(sm, tools.NewRegistry(), p)

	resp, err := loop.RunIteration(context.Background(), "s1", "hi")
	if err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if resp != "first answer" {
		t.Fatalf("want 'first answer', got %q", resp)
	}
	if got := len(p.capturedTools); got != 1 {
		t.Fatalf("want 1 LLM call, got %d", got)
	}
}

func TestReflect_RevisesFinalAnswer(t *testing.T) {
	// Turn 1: draft. Turns 2 and 3 are critique rounds that each produce a
	// different revised answer; the final answer must match the last round.
	p := &reflectProvider{turns: []string{"draft", "revised once", "revised twice"}}
	sm := history.NewInMemSessionManager("sys")
	loop := NewAgentLoop(sm, tools.NewRegistry(), p)
	loop.Reflect = 2

	resp, err := loop.RunIteration(context.Background(), "s1", "write sql")
	if err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if resp != "revised twice" {
		t.Fatalf("want 'revised twice', got %q", resp)
	}
	if got := len(p.capturedTools); got != 3 {
		t.Fatalf("want 3 LLM calls (1 draft + 2 critiques), got %d", got)
	}
	// Reflection rounds must be invoked without a tool registry.
	for i := 1; i < len(p.capturedTools); i++ {
		if p.capturedTools[i] {
			t.Fatalf("critique call %d received tool registry; expected nil", i)
		}
	}
	// History should carry the revised answer, not the draft.
	hist, _ := sm.History(context.Background(), "s1")
	last := hist[len(hist)-1]
	if last.Role != "assistant" || last.Content != "revised twice" {
		t.Fatalf("history last msg: %+v", last)
	}
}

func TestReflect_NoOpRoundLeavesAnswerIntact(t *testing.T) {
	// Model reaffirms the same answer — no ReflectedEvent should be emitted
	// because there's nothing to replace.
	p := &reflectProvider{turns: []string{"stable", "stable", "stable"}}
	sm := history.NewInMemSessionManager("sys")
	loop := NewAgentLoop(sm, tools.NewRegistry(), p)
	loop.Reflect = 2

	streamChan := make(chan StreamEvent, 64)
	go loop.runLogicLoop(context.Background(), "s1", history.Message{Role: "user", Content: "hi"}, streamChan)

	var reflectedCount int
	var finalContentBuf strings.Builder
	for ev := range streamChan {
		switch p := ev.Payload.(type) {
		case ReflectedEvent:
			_ = p
			reflectedCount++
		case ContentEvent:
			if ev.Source == "" {
				finalContentBuf.WriteString(p.Text)
			}
		}
	}
	if reflectedCount != 0 {
		t.Fatalf("unchanged answer must not emit reflected events, got %d", reflectedCount)
	}
	if finalContentBuf.String() != "stable" {
		t.Fatalf("final content should stay 'stable', got %q", finalContentBuf.String())
	}
}

func TestReflect_StreamsUnderReflectSource(t *testing.T) {
	// The critique round's content must flow through streamChan tagged with
	// Source="reflect:<round>" so streaming UIs can render it distinctly.
	p := &reflectProvider{turns: []string{"draft", "revised"}}
	sm := history.NewInMemSessionManager("sys")
	loop := NewAgentLoop(sm, tools.NewRegistry(), p)
	loop.Reflect = 1

	streamChan := make(chan StreamEvent, 64)
	go loop.runLogicLoop(context.Background(), "s1", history.Message{Role: "user", Content: "hi"}, streamChan)

	sawReflectSource := false
	sawReflectedEvent := false
	var reflectedPayload ReflectedEvent
	for ev := range streamChan {
		if ev.Type == EventTypeContent && strings.HasPrefix(ev.Source, "reflect:") {
			sawReflectSource = true
		}
		if ev.Type == EventTypeReflected {
			sawReflectedEvent = true
			if p, ok := ev.Payload.(ReflectedEvent); ok {
				reflectedPayload = p
			}
		}
	}
	if !sawReflectSource {
		t.Fatal("expected at least one content event with Source=reflect:*")
	}
	if !sawReflectedEvent {
		t.Fatal("expected a reflected event carrying canonical text")
	}
	if reflectedPayload.Text != "revised" || reflectedPayload.Round != 1 {
		t.Fatalf("reflected payload: %+v", reflectedPayload)
	}
}

func TestReflect_CustomPromptIsUsed(t *testing.T) {
	// ReflectPrompt override must appear as the last user message the
	// provider sees during a critique round.
	p := &capturingProvider{}
	sm := history.NewInMemSessionManager("sys")
	loop := NewAgentLoop(sm, tools.NewRegistry(), p)
	loop.Reflect = 1
	loop.ReflectPrompt = "CUSTOM_CHECK_ME"

	if _, err := loop.RunIteration(context.Background(), "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if len(p.seenMsgs) < 2 {
		t.Fatalf("need at least 2 turns, got %d", len(p.seenMsgs))
	}
	critique := p.seenMsgs[1]
	last := critique[len(critique)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "CUSTOM_CHECK_ME") {
		t.Fatalf("custom prompt not injected into critique turn: %+v", last)
	}
}

func TestReflect_ErrorAbortsButKeepsDraft(t *testing.T) {
	// First call succeeds (draft), second call (first critique) errors.
	// Outer loop should fall back to the draft and not wedge.
	p := &erroringReflectProvider{draft: "draft answer", errAt: 1, err: errors.New("transient boom")}
	sm := history.NewInMemSessionManager("sys")
	loop := NewAgentLoop(sm, tools.NewRegistry(), p)
	loop.Reflect = 2

	resp, err := loop.RunIteration(context.Background(), "s1", "hi")
	if err != nil {
		t.Fatalf("unexpected error from RunIteration: %v", err)
	}
	if resp != "draft answer" {
		t.Fatalf("want 'draft answer' after failed critique, got %q", resp)
	}
}

// --- fixtures ---

type capturingProvider struct {
	mu       sync.Mutex
	seenMsgs [][]history.Message
	turns    []string
	idx      int
}

func (p *capturingProvider) GenerateStream(_ context.Context, msgs []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.mu.Lock()
	cp := make([]history.Message, len(msgs))
	copy(cp, msgs)
	p.seenMsgs = append(p.seenMsgs, cp)
	text := "x"
	if p.idx < len(p.turns) {
		text = p.turns[p.idx]
		p.idx++
	}
	p.mu.Unlock()
	ch <- Event(ContentEvent{Text: text})
	return LLMResult{Content: text}, nil
}

type erroringReflectProvider struct {
	mu    sync.Mutex
	draft string
	count int
	errAt int
	err   error
}

func (p *erroringReflectProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.mu.Lock()
	n := p.count
	p.count++
	p.mu.Unlock()
	if n == p.errAt {
		return LLMResult{}, fmt.Errorf("erroringReflectProvider: %w", p.err)
	}
	ch <- Event(ContentEvent{Text: p.draft})
	return LLMResult{Content: p.draft}, nil
}

// --- Scorer: keep the best round, not the last one ---

// scoreByAnswer ranks candidates from a lookup table, so a test can make
// a later round strictly worse than an earlier one.
func scoreByAnswer(table map[string]float64) Scorer {
	return ScorerFunc(func(_ context.Context, r RunResult) (float64, error) {
		return table[r.Answer], nil
	})
}

func TestReflect_ScorerKeepsBestRoundNotLast(t *testing.T) {
	provider := &reflectProvider{turns: []string{"original", "good", "worse"}}
	loop, sm := setup(provider)
	loop.Reflect = 2
	loop.Scorer = scoreByAnswer(map[string]float64{
		"original": 10,
		"good":     90,
		"worse":    20,
	})

	got, err := loop.RunIteration(context.Background(), "s1", "q")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "good" {
		t.Fatalf("answer = %q, want the best-scoring round %q", got, "good")
	}

	msgs, err := sm.History(context.Background(), "s1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	final := msgs[len(msgs)-1]
	if final.Role != "assistant" || final.Content != "good" {
		t.Fatalf("persisted answer = %+v, want the kept round", final)
	}
}

func TestReflect_ScorerRejectsRegressionAndRecritiquesBest(t *testing.T) {
	provider := &reflectProvider{turns: []string{"original", "worse", "best"}}
	loop, _ := setup(provider)
	loop.Reflect = 2
	loop.Scorer = scoreByAnswer(map[string]float64{
		"original": 50,
		"worse":    10,
		"best":     99,
	})

	got, err := loop.RunIteration(context.Background(), "s1", "q")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Round 1 regressed and was discarded; round 2 beat the original.
	if got != "best" {
		t.Fatalf("answer = %q, want %q", got, "best")
	}
	// The rollback is the actual claim: round 2 must critique the
	// incumbent, not the revision that was just thrown away. Asserting
	// only the final answer passes even with the rollback deleted,
	// because "best" outscores the incumbent either way.
	provider.mu.Lock()
	critiqued := append([]string(nil), provider.critiqued...)
	provider.mu.Unlock()
	if len(critiqued) != 3 {
		t.Fatalf("want 3 provider calls (answer + 2 critiques), got %d: %q", len(critiqued), critiqued)
	}
	if critiqued[1] != "original" {
		t.Fatalf("round 1 critiqued %q, want the original answer", critiqued[1])
	}
	if critiqued[2] != "original" {
		t.Fatalf("round 2 critiqued %q — the rejected revision was not rolled back", critiqued[2])
	}
}

func TestReflect_ScorerTieKeepsIncumbent(t *testing.T) {
	provider := &reflectProvider{turns: []string{"original", "cosmetic"}}
	loop, _ := setup(provider)
	loop.Reflect = 1
	loop.Scorer = scoreByAnswer(map[string]float64{"original": 42, "cosmetic": 42})

	got, err := loop.RunIteration(context.Background(), "s1", "q")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "original" {
		t.Fatalf("answer = %q — a tie must not displace the incumbent", got)
	}
}

func TestReflect_ScorerErrorDiscardsRevision(t *testing.T) {
	provider := &reflectProvider{turns: []string{"original", "unrankable"}}
	loop, _ := setup(provider)
	loop.Reflect = 1
	loop.Scorer = ScorerFunc(func(_ context.Context, r RunResult) (float64, error) {
		if r.Round == 0 {
			return 5, nil
		}
		return 0, errors.New("judge unavailable")
	})

	got, err := loop.RunIteration(context.Background(), "s1", "q")
	if err != nil {
		t.Fatalf("scorer failure must not fail the turn, got %v", err)
	}
	if got != "original" {
		t.Fatalf("answer = %q — an unrankable revision cannot be shown to be better", got)
	}
}

func TestReflect_NoScorerKeepsLastWinsBehavior(t *testing.T) {
	provider := &reflectProvider{turns: []string{"original", "second", "third"}}
	loop, _ := setup(provider)
	loop.Reflect = 2

	got, err := loop.RunIteration(context.Background(), "s1", "q")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "third" {
		t.Fatalf("answer = %q, want last-wins %q without a Scorer", got, "third")
	}
}

func TestReflect_AdoptedRoundCarriesItsScore(t *testing.T) {
	provider := &reflectProvider{turns: []string{"original", "better"}}
	loop, _ := setup(provider)
	loop.Reflect = 1
	loop.Scorer = scoreByAnswer(map[string]float64{"original": 1, "better": 7})

	var reflected []ReflectedEvent
	for ev := range loop.RunText(context.Background(), "s1", "q") {
		if p, ok := ev.Payload.(ReflectedEvent); ok {
			reflected = append(reflected, p)
		}
	}
	if len(reflected) != 1 {
		t.Fatalf("want exactly one ReflectedEvent for the adopted round, got %d", len(reflected))
	}
	if reflected[0].Score == nil || *reflected[0].Score != 7 || reflected[0].Round != 1 {
		t.Fatalf("event = %+v, want round 1 scored 7", reflected[0])
	}
}
