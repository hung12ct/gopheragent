package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/cache"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// --- helpers ---

type echoTool struct {
	name    string
	confirm bool
}

func (t *echoTool) Name() string                          { return t.name }
func (t *echoTool) Description() string                   { return "echoes input" }
func (t *echoTool) ParametersSchema() tools.ToolSchema { return tools.ToolSchema{} }
func (t *echoTool) RequiresConfirmation() bool             { return t.confirm }
func (t *echoTool) Execute(_ context.Context, args string) (string, error) {
	return "echo:" + args, nil
}

type scriptProvider struct {
	turns []LLMResult
	idx   int
}

func (p *scriptProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	if p.idx >= len(p.turns) {
		return LLMResult{Content: "done"}, nil
	}
	r := p.turns[p.idx]
	p.idx++
	if len(r.ToolCalls) == 0 {
		ch <- StreamEvent{Type: "content", Content: r.Content}
	}
	return r, nil
}

func setup(provider LLMProvider, toolList ...tools.Tool) (*AgentLoop, *history.InMemSessionManager) {
	sm := history.NewInMemSessionManager("test system prompt")
	reg := tools.NewRegistry()
	for _, t := range toolList {
		reg.Register(t)
	}
	loop := NewAgentLoop(sm, reg, provider)
	return loop, sm
}

// --- tests ---

func TestRunIteration_DirectResponse(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{Content: "Hello world"},
	}}
	loop, _ := setup(provider)

	resp, err := loop.RunIteration(context.Background(), "s1", "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Hello world" {
		t.Fatalf("expected 'Hello world', got %q", resp)
	}
}

func TestRunIteration_ToolCallThenResponse(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "echo", ArgsJSON: `"test"`}}},
		{Content: "final answer"},
	}}
	loop, _ := setup(provider, &echoTool{name: "echo"})

	resp, err := loop.RunIteration(context.Background(), "s1", "do something")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "final answer" {
		t.Fatalf("expected 'final answer', got %q", resp)
	}
}

func TestRunIteration_ToolNotFound(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "nonexistent", ArgsJSON: `{}`}}},
		{Content: "fallback"},
	}}
	loop, _ := setup(provider)

	resp, err := loop.RunIteration(context.Background(), "s1", "try missing tool")
	// The loop emits an error event for the missing tool but continues to the next iteration.
	// RunIteration captures the last error AND the subsequent content.
	if err == nil {
		t.Fatal("expected an error event for missing tool")
	}
	if resp != "fallback" {
		t.Fatalf("expected 'fallback', got %q", resp)
	}
}

func TestRunIteration_HITLDenial(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "dangerous", ArgsJSON: `{}`}}},
		{Content: "ok I found another way"},
	}}
	loop, _ := setup(provider, &echoTool{name: "dangerous", confirm: true})

	resp, err := loop.RunIteration(context.Background(), "s1", "delete everything")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "ok I found another way" {
		t.Fatalf("expected fallback response, got %q", resp)
	}
}

func TestRunIteration_MaxIters(t *testing.T) {
	// Provider always returns a tool call — should hit max iterations
	provider := &scriptProvider{turns: make([]LLMResult, 20)}
	for i := range provider.turns {
		provider.turns[i] = LLMResult{ToolCalls: []PendingToolCall{{ID: fmt.Sprintf("c%d", i), Name: "echo", ArgsJSON: `"loop"`}}}
	}
	loop, _ := setup(provider, &echoTool{name: "echo"})
	loop.MaxIters = 3

	_, err := loop.RunIteration(context.Background(), "s1", "loop forever")
	if err == nil || !strings.Contains(err.Error(), "maximum iterations") {
		t.Fatalf("expected max iterations error, got: %v", err)
	}
}

func TestRunIteration_HookRejection(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{{Content: "should not reach"}}}
	loop, _ := setup(provider)
	loop.BeforeHooks = []Hook{
		func(_ context.Context, _ string, input string) error {
			if strings.Contains(input, "hack") {
				return fmt.Errorf("blocked")
			}
			return nil
		},
	}

	_, err := loop.RunIteration(context.Background(), "s1", "hack the system")
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected hook rejection error, got: %v", err)
	}
}

func TestRunIteration_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	provider := &scriptProvider{turns: []LLMResult{{Content: "unreachable"}}}
	loop, _ := setup(provider)

	_, err := loop.RunIteration(ctx, "s1", "hello")
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected cancellation error, got: %v", err)
	}
}

func TestRunIteration_ParallelToolCalls(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{
			{ID: "c1", Name: "echo", ArgsJSON: `"a"`},
			{ID: "c2", Name: "echo", ArgsJSON: `"b"`},
		}},
		{Content: "both done"},
	}}
	loop, _ := setup(provider, &echoTool{name: "echo"})

	resp, err := loop.RunIteration(context.Background(), "s1", "do two things")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "both done" {
		t.Fatalf("expected 'both done', got %q", resp)
	}
}

func TestRunIterationStream_EventTypes(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "echo", ArgsJSON: `"x"`}}},
		{Content: "streamed"},
	}}
	loop, _ := setup(provider, &echoTool{name: "echo"})
	loop.EmitThoughts = true

	ch := make(chan StreamEvent, 100)
	loop.RunIterationStream(context.Background(), "s1", "hi", ch)

	types := map[StreamEventType]bool{}
	for ev := range ch {
		types[ev.Type] = true
	}
	for _, want := range []StreamEventType{EventTypeToolCall, EventTypeContent, EventTypeDone} {
		if !types[want] {
			t.Errorf("expected event type %q in stream, got types: %v", want, types)
		}
	}
}

func TestSessionHistoryPersisted(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{{Content: "saved"}}}
	loop, sm := setup(provider)

	_, err := loop.RunIteration(context.Background(), "s1", "persist me")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := sm.GetHistory(context.Background(), "s1")
	if len(msgs) < 3 { // system + user + assistant
		t.Fatalf("expected at least 3 messages in history, got %d", len(msgs))
	}
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" || last.Content != "saved" {
		t.Fatalf("expected last message to be assistant 'saved', got role=%q content=%q", last.Role, last.Content)
	}
}

func TestRunIteration_HITLApproved(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "dangerous", ArgsJSON: `{}`}}},
		{Content: "executed successfully"},
	}}
	loop, _ := setup(provider, &echoTool{name: "dangerous", confirm: true})
	loop.ConfirmHITL = func(_ context.Context, toolName, argsJSON string) bool {
		return true // approve
	}

	resp, err := loop.RunIteration(context.Background(), "s1", "do risky thing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "executed successfully" {
		t.Fatalf("expected tool to execute after approval, got %q", resp)
	}
}

func TestRunIteration_HITLDeniedViaCallback(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "dangerous", ArgsJSON: `{}`}}},
		{Content: "found workaround"},
	}}
	loop, _ := setup(provider, &echoTool{name: "dangerous", confirm: true})
	loop.ConfirmHITL = func(_ context.Context, toolName, argsJSON string) bool {
		return false // deny
	}

	resp, err := loop.RunIteration(context.Background(), "s1", "do risky thing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "found workaround" {
		t.Fatalf("expected denial → workaround, got %q", resp)
	}
}

func TestRunIteration_CacheHit(t *testing.T) {
	callCount := 0
	countingTool := &countingEchoTool{name: "echo", counter: &callCount}

	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "echo", ArgsJSON: `"hello"`}}},
		{Content: "first done"},
		{ToolCalls: []PendingToolCall{{ID: "c2", Name: "echo", ArgsJSON: `"hello"`}}},
		{Content: "second done"},
	}}

	sm := history.NewInMemSessionManager("test")
	reg := tools.NewRegistry()
	reg.Register(countingTool)
	loop := NewAgentLoop(sm, reg, provider)
	loop.Cache = cache.NewSearchCache(100, 5*time.Minute)

	// First run — tool should execute
	resp1, _ := loop.RunIteration(context.Background(), "s1", "call echo")
	if resp1 != "first done" {
		t.Fatalf("expected 'first done', got %q", resp1)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 tool execution, got %d", callCount)
	}

	// Reset provider for second run
	provider.idx = 2
	resp2, _ := loop.RunIteration(context.Background(), "s2", "call echo again")
	if resp2 != "second done" {
		t.Fatalf("expected 'second done', got %q", resp2)
	}
	if callCount != 1 { // should NOT increment — cache hit
		t.Fatalf("expected cache hit (still 1 execution), got %d", callCount)
	}
}

func TestRunIteration_LLMError(t *testing.T) {
	provider := &errorProvider{err: fmt.Errorf("model overloaded")}
	loop, sm := setup(provider)

	_, err := loop.RunIteration(context.Background(), "s1", "trigger error")
	if err == nil || !strings.Contains(err.Error(), "model overloaded") {
		t.Fatalf("expected LLM error propagation, got: %v", err)
	}
	// History should be saved even on LLM error (user message preserved)
	msgs := sm.GetHistory(context.Background(), "s1")
	if len(msgs) < 2 { // system + user
		t.Fatalf("expected history to be saved on LLM error, got %d messages", len(msgs))
	}
}

func TestRunIteration_ConcurrentSessions(t *testing.T) {
	sm := history.NewInMemSessionManager("test")
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})

	const N = 10
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			provider := &scriptProvider{turns: []LLMResult{
				{ToolCalls: []PendingToolCall{{ID: fmt.Sprintf("c%d", i), Name: "echo", ArgsJSON: `"hi"`}}},
				{Content: fmt.Sprintf("resp_%d", i)},
			}}
			loop := NewAgentLoop(sm, reg, provider)
			sessionKey := fmt.Sprintf("session_%d", i)
			resp, err := loop.RunIteration(context.Background(), sessionKey, fmt.Sprintf("msg_%d", i))
			if err != nil {
				errs <- fmt.Errorf("session %d error: %w", i, err)
				return
			}
			if resp != fmt.Sprintf("resp_%d", i) {
				errs <- fmt.Errorf("session %d: expected 'resp_%d', got %q", i, i, resp)
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < N; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunIteration_EventHandler(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "echo", ArgsJSON: `"x"`}}},
		{Content: "done"},
	}}
	loop, _ := setup(provider, &echoTool{name: "echo"})

	var seen []StreamEventType
	loop.OnEvent(func(_ context.Context, _ string, ev StreamEvent) {
		seen = append(seen, ev.Type)
	})

	_, err := loop.RunIteration(context.Background(), "s1", "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantTypes := map[StreamEventType]bool{EventTypeToolCall: false, EventTypeContent: false, EventTypeDone: false}
	for _, typ := range seen {
		wantTypes[typ] = true
	}
	for typ, found := range wantTypes {
		if !found {
			t.Errorf("EventHandler never received event type %q; got: %v", typ, seen)
		}
	}
}

func TestRunIteration_EventHandlerMultiple(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{{Content: "ok"}}}
	loop, _ := setup(provider)

	count1, count2 := 0, 0
	loop.OnEvent(func(_ context.Context, _ string, _ StreamEvent) { count1++ }).
		OnEvent(func(_ context.Context, _ string, _ StreamEvent) { count2++ })

	loop.RunIteration(context.Background(), "s1", "hi")
	if count1 == 0 || count1 != count2 {
		t.Fatalf("both handlers should fire equally, got count1=%d count2=%d", count1, count2)
	}
}

func TestRunIteration_RetryOnLLMError(t *testing.T) {
	attempts := 0
	provider := &countingErrorProvider{
		failN: 2, // fail first 2 attempts, succeed on 3rd
		onCall: func() { attempts++ },
		successResult: LLMResult{Content: "recovered"},
	}
	loop, _ := setup(provider)
	loop.Retry = &RetryConfig{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}

	resp, err := loop.RunIteration(context.Background(), "s1", "hi")
	if err != nil {
		t.Fatalf("expected recovery after retry, got: %v", err)
	}
	if resp != "recovered" {
		t.Fatalf("expected 'recovered', got %q", resp)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 LLM calls (2 fail + 1 success), got %d", attempts)
	}
}

func TestRunIteration_NoRetryOnContextCancel(t *testing.T) {
	attempts := 0
	ctx, cancel := context.WithCancel(context.Background())

	provider := &countingErrorProvider{
		failN: 5,
		onCall: func() {
			attempts++
			cancel() // cancel on first call
		},
	}
	loop, _ := setup(provider)
	loop.Retry = &RetryConfig{MaxRetries: 3, BaseDelay: time.Millisecond}

	_, err := loop.RunIteration(ctx, "s1", "hi")
	if err == nil {
		t.Fatal("expected error after context cancel")
	}
	// Should not retry after context cancelled
	if attempts > 1 {
		t.Fatalf("expected no retry after context cancel, got %d attempts", attempts)
	}
}

func TestRunIteration_StructuredError_MaxIters(t *testing.T) {
	provider := &scriptProvider{turns: make([]LLMResult, 20)}
	for i := range provider.turns {
		provider.turns[i] = LLMResult{ToolCalls: []PendingToolCall{{ID: fmt.Sprintf("c%d", i), Name: "echo", ArgsJSON: `"x"`}}}
	}
	loop, _ := setup(provider, &echoTool{name: "echo"})
	loop.MaxIters = 2

	_, err := loop.RunIteration(context.Background(), "s1", "loop")
	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("expected ErrMaxIterations, got: %v (type %T)", err, err)
	}
}

func TestRunIteration_StructuredError_ToolNotFound(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "ghost", ArgsJSON: `{}`}}},
		{Content: "fallback"},
	}}
	loop, _ := setup(provider)

	_, err := loop.RunIteration(context.Background(), "s1", "call ghost")
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("expected ErrToolNotFound, got: %v (type %T)", err, err)
	}
	var tfe *ToolNotFoundError
	if !errors.As(err, &tfe) || tfe.ToolName != "ghost" {
		t.Fatalf("expected ToolNotFoundError{ToolName:'ghost'}, got: %v", err)
	}
}

func TestRunIteration_StructuredError_HookRejected(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{{Content: "unreachable"}}}
	loop, _ := setup(provider)
	loop.BeforeHooks = []Hook{func(_ context.Context, _ string, _ string) error {
		return fmt.Errorf("blocked by policy")
	}}

	_, err := loop.RunIteration(context.Background(), "s1", "hi")
	if !errors.Is(err, ErrHookRejected) {
		t.Fatalf("expected ErrHookRejected, got: %v (type %T)", err, err)
	}
	var hre *HookRejectedError
	if !errors.As(err, &hre) || !strings.Contains(hre.Cause.Error(), "blocked by policy") {
		t.Fatalf("expected HookRejectedError with cause, got: %v", err)
	}
}

func TestRunIteration_StructuredError_LLMFailure(t *testing.T) {
	inner := fmt.Errorf("rate limit exceeded")
	provider := &errorProvider{err: inner}
	loop, _ := setup(provider)

	_, err := loop.RunIteration(context.Background(), "s1", "hi")
	if !errors.Is(err, ErrLLMFailure) {
		t.Fatalf("expected ErrLLMFailure, got: %v (type %T)", err, err)
	}
	var lfe *LLMFailureError
	if !errors.As(err, &lfe) || !strings.Contains(lfe.Cause.Error(), "rate limit") {
		t.Fatalf("expected LLMFailureError with inner cause, got: %v", err)
	}
}

func TestRunIteration_TokenBudgetPrune(t *testing.T) {
	var pruneTriggered bool
	provider := &scriptProvider{turns: []LLMResult{{Content: "answer"}}}
	loop, _ := setup(provider)
	loop.MaxTokenBudget = 1 // ridiculously low → always triggers
	loop.OnEvent(func(_ context.Context, _ string, ev StreamEvent) {
		if ev.Type == "thought" && strings.Contains(ev.Content, "Token budget") {
			pruneTriggered = true
		}
	})

	loop.RunIteration(context.Background(), "s1", "hello")
	if !pruneTriggered {
		t.Fatal("expected token budget prune thought event")
	}
}

// --- helpers ---

type errorProvider struct {
	err error
}

func (p *errorProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- StreamEvent) (LLMResult, error) {
	return LLMResult{}, p.err
}

// countingErrorProvider fails the first failN calls, then returns successResult.
type countingErrorProvider struct {
	failN         int
	onCall        func()
	successResult LLMResult
	calls         int
}

func (p *countingErrorProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.calls++
	if p.onCall != nil {
		p.onCall()
	}
	if p.calls <= p.failN {
		return LLMResult{}, fmt.Errorf("transient error #%d", p.calls)
	}
	if len(p.successResult.ToolCalls) == 0 {
		ch <- StreamEvent{Type: "content", Content: p.successResult.Content}
	}
	return p.successResult, nil
}

type countingEchoTool struct {
	name    string
	counter *int
}

func (t *countingEchoTool) Name() string                              { return t.name }
func (t *countingEchoTool) Description() string                       { return "counting echo" }
func (t *countingEchoTool) ParametersSchema() tools.ToolSchema  { return tools.ToolSchema{} }
func (t *countingEchoTool) RequiresConfirmation() bool                { return false }
func (t *countingEchoTool) Execute(_ context.Context, args string) (string, error) {
	*t.counter++
	return "echo:" + args, nil
}
func (t *countingEchoTool) Cacheable() bool { return true }
