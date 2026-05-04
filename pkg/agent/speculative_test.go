package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// --- shouldSpeculate eligibility matrix ---

func TestShouldSpeculate_DisabledByDefault(t *testing.T) {
	loop, _ := setup(&scriptProvider{}, &echoTool{name: "echo"})
	// SpeculativeTools defaults to false
	if loop.shouldSpeculate("s1", "c1", "echo", `{"x":1}`) {
		t.Fatal("speculation must stay off until SpeculativeTools is opted in")
	}
}

func TestShouldSpeculate_OptedIn(t *testing.T) {
	loop, _ := setup(&scriptProvider{}, &echoTool{name: "echo"})
	loop.SpeculativeTools = true
	if !loop.shouldSpeculate("s1", "c1", "echo", `{"x":1}`) {
		t.Fatal("expected eligibility for a safe tool with plain args")
	}
}

func TestShouldSpeculate_PlanModeBlocks(t *testing.T) {
	loop, _ := setup(&scriptProvider{}, &echoTool{name: "echo"})
	loop.SpeculativeTools = true
	loop.SetPlanMode("s1", true)
	if loop.shouldSpeculate("s1", "c1", "echo", `{}`) {
		t.Fatal("plan mode must suppress speculation entirely")
	}
}

func TestShouldSpeculate_ExitPlanModeSentinel(t *testing.T) {
	loop, _ := setup(&scriptProvider{})
	loop.SpeculativeTools = true
	if loop.shouldSpeculate("s1", "c1", ExitPlanModeToolName, `{}`) {
		t.Fatal("exit_plan_mode is a loop-level sentinel and must never be speculated")
	}
}

func TestShouldSpeculate_HITLToolBlocks(t *testing.T) {
	loop, _ := setup(&scriptProvider{}, &echoTool{name: "dangerous", confirm: true})
	loop.SpeculativeTools = true
	if loop.shouldSpeculate("s1", "c1", "dangerous", `{}`) {
		t.Fatal("HITL-gated tools require human approval before running — no speculation allowed")
	}
}

func TestShouldSpeculate_OutputOfReferenceBlocks(t *testing.T) {
	loop, _ := setup(&scriptProvider{}, &echoTool{name: "echo"})
	loop.SpeculativeTools = true
	if loop.shouldSpeculate("s1", "c1", "echo", `{"prev":"<output_of:c0.result>"}`) {
		t.Fatal("args with <output_of:...> get substituted post-stream — speculating would use unresolved placeholders")
	}
}

func TestShouldSpeculate_UnknownToolBlocks(t *testing.T) {
	loop, _ := setup(&scriptProvider{}) // no tools registered
	loop.SpeculativeTools = true
	if loop.shouldSpeculate("s1", "c1", "ghost", `{}`) {
		t.Fatal("tools not in the registry cannot be speculated")
	}
}

// --- spawnSpeculative + awaitSpeculative ---

func TestSpawnSpeculative_CachesResult(t *testing.T) {
	loop, _ := setup(&scriptProvider{}, &echoTool{name: "echo"})
	loop.SpeculativeTools = true

	store := newSpeculativeMap()
	var mu sync.Mutex

	loop.spawnSpeculative(context.Background(), "c1", "echo", `"hi"`, &mu, store)

	mu.Lock()
	sm, ok := store["c1"]
	mu.Unlock()
	if !ok {
		t.Fatal("speculative entry should be registered before the goroutine runs")
	}

	result, _, err := awaitSpeculative(context.Background(), sm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != `echo:"hi"` {
		t.Fatalf("expected echo result, got %q", result)
	}
}

// TestSpawnSpeculative_CancelAbortsTool pins the retry-orphan fix:
// calling sm.cancel() must propagate ctx cancellation to the running
// tool so it can abort early instead of completing with an unread
// result. Uses a tool that blocks on ctx.Done.
func TestSpawnSpeculative_CancelAbortsTool(t *testing.T) {
	tool := &ctxBlockingTool{name: "blocker", started: make(chan struct{})}
	loop, _ := setup(&scriptProvider{}, tool)
	loop.SpeculativeTools = true

	store := newSpeculativeMap()
	var mu sync.Mutex

	loop.spawnSpeculative(context.Background(), "c1", "blocker", `{}`, &mu, store)

	mu.Lock()
	sm := store["c1"]
	mu.Unlock()

	// Wait for the tool to start so we know it's actually blocked on ctx.
	<-tool.started

	// Simulate retry-reset: cancel the speculation while the tool is mid-run.
	sm.cancel()

	// Tool must abort with ctx.Canceled rather than completing.
	_, _, err := awaitSpeculative(context.Background(), sm)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected ctx.Canceled after sm.cancel(), got %v", err)
	}
}

// ctxBlockingTool blocks until ctx cancels, then returns ctx.Err().
type ctxBlockingTool struct {
	name    string
	started chan struct{}
}

func (t *ctxBlockingTool) Name() string                       { return t.name }
func (t *ctxBlockingTool) Description() string                { return "blocks until ctx cancels" }
func (t *ctxBlockingTool) ParametersSchema() tools.ToolSchema { return tools.ToolSchema{} }
func (t *ctxBlockingTool) RequiresConfirmation() bool         { return false }
func (t *ctxBlockingTool) Display() tools.ToolDisplay         { return tools.DefaultDisplay(t.Name(), t.Description()) }
func (t *ctxBlockingTool) Execute(ctx context.Context, _ string) (string, error) {
	close(t.started)
	<-ctx.Done()
	return "", ctx.Err()
}

func TestSpawnSpeculative_MissingToolSurfacesError(t *testing.T) {
	loop, _ := setup(&scriptProvider{}) // no tools
	loop.SpeculativeTools = true

	store := newSpeculativeMap()
	var mu sync.Mutex

	loop.spawnSpeculative(context.Background(), "c1", "ghost", `{}`, &mu, store)

	mu.Lock()
	sm := store["c1"]
	mu.Unlock()
	_, _, err := awaitSpeculative(context.Background(), sm)
	if err == nil {
		t.Fatal("expected ToolNotFoundError from speculative execution")
	}
}

func TestAwaitSpeculative_ContextCancelReturnsEarly(t *testing.T) {
	// A slow tool that never finishes on its own.
	sm := &speculativeExec{id: "c1", doneCh: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	_, _, err := awaitSpeculative(ctx, sm)
	if err == nil {
		t.Fatal("expected ctx.Err() when caller cancels before speculative done")
	}
}

// --- end-to-end: tool_call_ready → speculative exec → reuse in wave ---

// streamingToolCallReadyProvider emits a tool_call_ready event mid-stream
// on turn 0 and waits briefly before returning the LLMResult so the drainer
// has time to kick off the speculation. Turn 1 returns a plain final answer.
type streamingToolCallReadyProvider struct {
	turn     int
	toolName string
	argsJSON string
}

func (p *streamingToolCallReadyProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	if p.turn == 0 {
		payload, _ := json.Marshal(struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Args string `json:"args"`
		}{ID: "c1", Name: p.toolName, Args: p.argsJSON})
		ch <- StreamEvent{Type: EventTypeToolCallReady, Content: string(payload)}
		// Let the drainer observe the event and spawn the speculation
		// before we return the tool-call result.
		time.Sleep(20 * time.Millisecond)
		p.turn++
		return LLMResult{ToolCalls: []PendingToolCall{{ID: "c1", Name: p.toolName, ArgsJSON: p.argsJSON}}}, nil
	}
	ch <- StreamEvent{Type: "content", Content: "final answer"}
	p.turn++
	return LLMResult{Content: "final answer"}, nil
}

// slowCountingTool blocks for delay then returns; counts invocations.
type slowCountingTool struct {
	name    string
	delay   time.Duration
	counter *int64
}

func (t *slowCountingTool) Name() string                        { return t.name }
func (t *slowCountingTool) Description() string                 { return "slow counting tool" }
func (t *slowCountingTool) ParametersSchema() tools.ToolSchema  { return tools.ToolSchema{} }
func (t *slowCountingTool) RequiresConfirmation() bool          { return false }
func (t *slowCountingTool) Display() tools.ToolDisplay { return tools.DefaultDisplay(t.Name(), t.Description()) }
func (t *slowCountingTool) Execute(ctx context.Context, args string) (string, error) {
	atomic.AddInt64(t.counter, 1)
	select {
	case <-time.After(t.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "result:" + args, nil
}

func TestSpeculative_ToolExecutedOnceAndReused(t *testing.T) {
	var count int64
	tool := &slowCountingTool{name: "slow", delay: 10 * time.Millisecond, counter: &count}

	provider := &streamingToolCallReadyProvider{toolName: "slow", argsJSON: `"x"`}
	loop, _ := setup(provider, tool)
	loop.SpeculativeTools = true

	resp, err := loop.RunIteration(context.Background(), "s1", "go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "final answer" {
		t.Fatalf("expected 'final answer', got %q", resp)
	}
	// The tool must run exactly once even though two code paths (drainer +
	// wave executor) appear to call it — the wave executor should observe the
	// speculative entry and reuse it.
	if got := atomic.LoadInt64(&count); got != 1 {
		t.Fatalf("expected exactly 1 tool invocation, got %d", got)
	}
}

// TestSpeculative_ToolCallEventFlagsReused pins the v0.19.0 wire signal:
// when the wave executor consumes a speculative result instead of dispatching
// the tool again, the corresponding tool_call event has Reused=true so
// adopters can attribute the speculation savings.
func TestSpeculative_ToolCallEventFlagsReused(t *testing.T) {
	tool := &slowCountingTool{name: "slow", delay: 5 * time.Millisecond, counter: new(int64)}
	provider := &streamingToolCallReadyProvider{toolName: "slow", argsJSON: `"x"`}
	loop, _ := setup(provider, tool)
	loop.SpeculativeTools = true

	var sawReused bool
	var mu sync.Mutex
	loop.OnEvent(func(_ context.Context, _ string, ev StreamEvent) {
		if ev.Type != EventTypeToolCall {
			return
		}
		mu.Lock()
		if ev.Reused {
			sawReused = true
		}
		mu.Unlock()
	})

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawReused {
		t.Fatal("speculated dispatch did not flag Reused on the tool_call event")
	}
}

// TestSpeculative_StructuredPayloadReachesHook pins the speculator's
// StructuredResult dispatch: when SpeculativeTools is on and the tool
// implements tools.StructuredResult, the typed payload must still arrive
// at OnToolResult. Before the fix, the speculator only called Execute,
// so any speculated structured tool dropped its payload silently.
func TestSpeculative_StructuredPayloadReachesHook(t *testing.T) {
	tool := &structuredCounterTool{name: "structured"}
	provider := &streamingToolCallReadyProvider{toolName: "structured", argsJSON: `{}`}
	loop, _ := setup(provider, tool)
	loop.SpeculativeTools = true

	var seen any
	var mu sync.Mutex
	loop.OnToolResult = func(_ context.Context, _, _, _, result string, structured any, _ error) (string, error) {
		mu.Lock()
		seen = structured
		mu.Unlock()
		return result, nil
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	p, ok := seen.(SQLPayload)
	if !ok {
		t.Fatalf("speculated structured tool dropped its payload — got %T (%+v)", seen, seen)
	}
	if p.RowCount != 42 {
		t.Fatalf("payload not round-tripped through speculation: %+v", p)
	}
}

func TestSpeculative_OffRunsToolOnceNormally(t *testing.T) {
	var count int64
	tool := &slowCountingTool{name: "slow", delay: 5 * time.Millisecond, counter: &count}

	provider := &streamingToolCallReadyProvider{toolName: "slow", argsJSON: `"y"`}
	loop, _ := setup(provider, tool)
	// SpeculativeTools is off — drainer sees the event but does nothing.

	resp, err := loop.RunIteration(context.Background(), "s1", "go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "final answer" {
		t.Fatalf("expected 'final answer', got %q", resp)
	}
	if got := atomic.LoadInt64(&count); got != 1 {
		t.Fatalf("expected 1 tool invocation without speculation, got %d", got)
	}
}

func TestSpeculative_RetryClearsStaleMap(t *testing.T) {
	// Attempt 1 emits tool_call_ready (spawning a speculation), then the
	// provider returns a transient error. Attempt 2 (retry) re-emits the
	// same tool call and must NOT inherit attempt 1's speculative entry —
	// if it did, the wave executor would reuse a result tied to a failed
	// LLM turn. We assert count==2: attempt 1's orphaned speculation ran
	// plus attempt 2's fresh speculation (which the wave executor reuses).
	var count int64
	tool := &slowCountingTool{name: "slow", delay: 2 * time.Millisecond, counter: &count}

	provider := &retryableToolReadyProvider{toolName: "slow", argsJSON: `"z"`}
	loop, _ := setup(provider, tool)
	loop.SpeculativeTools = true
	loop.Retry = &RetryConfig{MaxRetries: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}

	resp, err := loop.RunIteration(context.Background(), "s1", "go")
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if resp != "final answer" {
		t.Fatalf("expected 'final answer', got %q", resp)
	}
	if got := atomic.LoadInt64(&count); got != 2 {
		t.Fatalf("expected 2 invocations (attempt-1 orphaned speculation + attempt-2 fresh), got %d", got)
	}
}

// retryableToolReadyProvider fails on turn 0, then on turn 1 emits
// tool_call_ready and returns the tool call, then on turn 2 returns final.
type retryableToolReadyProvider struct {
	calls    int
	toolName string
	argsJSON string
}

func (p *retryableToolReadyProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.calls++
	switch p.calls {
	case 1:
		// Emit a tool_call_ready that *would* have spawned a speculation,
		// then fail transiently so a retry happens.
		payload, _ := json.Marshal(struct {
			ID, Name, Args string
		}{"c1", p.toolName, p.argsJSON})
		ch <- StreamEvent{Type: EventTypeToolCallReady, Content: string(payload)}
		return LLMResult{}, fmt.Errorf("transient: rate limited")
	case 2:
		payload, _ := json.Marshal(struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Args string `json:"args"`
		}{"c1", p.toolName, p.argsJSON})
		ch <- StreamEvent{Type: EventTypeToolCallReady, Content: string(payload)}
		time.Sleep(5 * time.Millisecond)
		return LLMResult{ToolCalls: []PendingToolCall{{ID: "c1", Name: p.toolName, ArgsJSON: p.argsJSON}}}, nil
	default:
		ch <- StreamEvent{Type: "content", Content: "final answer"}
		return LLMResult{Content: "final answer"}, nil
	}
}
