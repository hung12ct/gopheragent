package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

func TestOnToolResult_RewritesSuccessResult(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(1)},
		{Content: "final"},
	}}
	loop, sm := setup(provider, ct)

	loop.OnToolResult = func(_ context.Context, _, name, _, result string, _ any, _ error) (string, error) {
		if name != "counter" {
			t.Errorf("hook saw unexpected tool name: %q", name)
		}
		return strings.ReplaceAll(result, "ok:", "rewritten:"), nil
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs, _ := sm.History(context.Background(), "s1")
	var seenRewritten bool
	for _, m := range msgs {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "rewritten:") {
			seenRewritten = true
		}
	}
	if !seenRewritten {
		t.Fatalf("rewritten tool result never reached the LLM context — msgs: %+v", msgs)
	}
}

func TestOnToolResult_VetoConvertsToToolError(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(1)},
		{Content: "final"},
	}}
	loop, sm := setup(provider, ct)

	veto := errors.New("post-validation rejected the output")
	loop.OnToolResult = func(_ context.Context, _, _, _, _ string, _ any, _ error) (string, error) {
		return "", veto
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs, _ := sm.History(context.Background(), "s1")
	var sawError bool
	for _, m := range msgs {
		if m.Role == "tool" && m.IsError && strings.Contains(m.Content, "post-validation rejected") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("hook veto never produced a tool-error message in history — msgs: %+v", msgs)
	}
}

// boomTool always fails. Used to pin OnToolResult error-path firing.
type boomTool struct{ name string }

func (f *boomTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        f.name,
		Description: "always fails",
		Display:     tools.DefaultDisplay(f.name, "always fails"),
	}
}

func (f *boomTool) Execute(_ context.Context, _ string) (tools.Result, error) {
	return tools.Result{}, errors.New("tool exploded")
}

// TestOnToolResult_FiresOnError pins the v0.19.0 contract: the hook now
// fires on error paths so adopters get every call (success or fail) in one
// place, eliminating the need for a separate EventTypeError listener.
func TestOnToolResult_FiresOnError(t *testing.T) {
	tool := &boomTool{name: "boom"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "boom", ArgsJSON: "{}"}}},
		{Content: "final"},
	}}
	loop, _ := setup(provider, tool)

	var sawErr error
	var sawCallID string
	var mu sync.Mutex
	loop.OnToolResult = func(_ context.Context, callID, _, _, _ string, _ any, execErr error) (string, error) {
		mu.Lock()
		sawErr = execErr
		sawCallID = callID
		mu.Unlock()
		return "", nil
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if sawErr == nil || !strings.Contains(sawErr.Error(), "tool exploded") {
		t.Fatalf("hook did not see the tool error: %v", sawErr)
	}
	if sawCallID == "" {
		t.Fatal("hook did not receive a correlation ID on the error path")
	}
}

// TestOnToolResult_HookCanRecoverError pins the recovery semantics: when the
// hook sees an error and returns (rewritten, nil), the call becomes a
// successful one with rewritten as the LLM-facing result.
func TestOnToolResult_HookCanRecoverError(t *testing.T) {
	tool := &boomTool{name: "boom"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "boom", ArgsJSON: "{}"}}},
		{Content: "final"},
	}}
	loop, sm := setup(provider, tool)

	loop.OnToolResult = func(_ context.Context, _, _, _, _ string, _ any, execErr error) (string, error) {
		if execErr != nil {
			return "fallback: tool unavailable, using cached value", nil
		}
		return "", nil
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs, _ := sm.History(context.Background(), "s1")
	var recovered bool
	for _, m := range msgs {
		if m.Role == "tool" && !m.IsError && strings.Contains(m.Content, "fallback") {
			recovered = true
		}
	}
	if !recovered {
		t.Fatalf("hook recovery never reached the LLM context — msgs: %+v", msgs)
	}
}

// TestOnToolResult_CallIDMatchesEvent pins the correlation contract: the
// toolCallID parameter handed to the hook is the same ID emitted on the
// preceding EventTypeToolCall, so adopters can pair entry events to
// post-execution audit lines.
func TestOnToolResult_CallIDMatchesEvent(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(1)},
		{Content: "final"},
	}}
	loop, _ := setup(provider, ct)

	var eventID, hookID string
	var mu sync.Mutex
	loop.OnEvent(func(_ context.Context, _ string, ev StreamEvent) {
		p, ok := ev.Payload.(ToolCallEvent)
		if !ok {
			return
		}
		mu.Lock()
		eventID = p.ID
		mu.Unlock()
	})
	loop.OnToolResult = func(_ context.Context, callID, _, _, _ string, _ any, _ error) (string, error) {
		mu.Lock()
		hookID = callID
		mu.Unlock()
		return "", nil
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if eventID == "" {
		t.Fatal("EventTypeToolCall did not carry a ToolCallID")
	}
	if eventID != hookID {
		t.Fatalf("event/hook IDs did not match: event=%q hook=%q", eventID, hookID)
	}
}

// idCapturingTool records the ToolCallID it sees on ctx so tests can assert
// the agent loop threaded the correlation ID into the per-tool context.
type idCapturingTool struct {
	name   string
	seenID string
	mu     sync.Mutex
}

func (i *idCapturingTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        i.name,
		Description: "captures ctx id",
		Display:     tools.DefaultDisplay(i.name, "captures ctx id"),
	}
}

func (i *idCapturingTool) Execute(ctx context.Context, _ string) (tools.Result, error) {
	i.mu.Lock()
	i.seenID = tools.ToolCallIDFromContext(ctx)
	i.mu.Unlock()
	return tools.Text("ok"), nil
}

// TestOnToolResult_ToolCtxCarriesCallID pins the ctx-threading contract:
// tools.ToolCallIDFromContext returns the same ID the hook receives, so
// middleware (e.g. WithLogging) can correlate without re-plumbing.
func TestOnToolResult_ToolCtxCarriesCallID(t *testing.T) {
	tool := &idCapturingTool{name: "capture"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "capture", ArgsJSON: "{}"}}},
		{Content: "final"},
	}}
	loop, _ := setup(provider, tool)

	var hookID string
	loop.OnToolResult = func(_ context.Context, callID, _, _, _ string, _ any, _ error) (string, error) {
		hookID = callID
		return "", nil
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tool.mu.Lock()
	defer tool.mu.Unlock()
	if tool.seenID == "" {
		t.Fatal("tool did not see a ToolCallID on ctx")
	}
	if tool.seenID != hookID {
		t.Fatalf("ctx ID and hook ID diverged: ctx=%q hook=%q", tool.seenID, hookID)
	}
}

// progressEmittingTool calls ReportProgress once during Execute. Used to
// verify that the loop attaches Name+ToolCallID to every progress event.
type progressEmittingTool struct{ name string }

func (p *progressEmittingTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        p.name,
		Description: "emits one progress message",
		Display:     tools.DefaultDisplay(p.name, "emits one progress message"),
	}
}

func (p *progressEmittingTool) Execute(ctx context.Context, _ string) (tools.Result, error) {
	tools.ReportProgress(ctx, "halfway there")
	return tools.Text("ok"), nil
}

// TestToolProgress_EventCarriesCallID pins the parallel-correlation contract
// end-to-end: a progress event emitted from inside a running tool carries
// the same Name and ToolCallID that landed on the originating tool_call
// event, so adopters can attribute progress to a specific dispatch even
// when SpeculativeTools=true interleaves multiple tools.
func TestToolProgress_EventCarriesCallID(t *testing.T) {
	tool := &progressEmittingTool{name: "longrun"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "longrun", ArgsJSON: "{}"}}},
		{Content: "final"},
	}}
	loop, _ := setup(provider, tool)

	var callEventID, progressEventID, progressEventName string
	var mu sync.Mutex
	loop.OnEvent(func(_ context.Context, _ string, ev StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		switch p := ev.Payload.(type) {
		case ToolCallEvent:
			callEventID = p.ID
		case ToolProgressEvent:
			progressEventID = p.ToolCallID
			progressEventName = p.Name
		}
	})

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if callEventID == "" {
		t.Fatal("tool_call event missing ToolCallID")
	}
	if progressEventID != callEventID {
		t.Fatalf("progress ToolCallID did not match tool_call: progress=%q call=%q", progressEventID, callEventID)
	}
	if progressEventName != "longrun" {
		t.Fatalf("progress Name did not match tool: got %q", progressEventName)
	}
}

func TestOnToolResult_NilHookIsZeroCost(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(1)},
		{Content: "final"},
	}}
	loop, sm := setup(provider, ct)
	// loop.OnToolResult left nil

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs, _ := sm.History(context.Background(), "s1")
	var sawOk bool
	for _, m := range msgs {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "ok:") {
			sawOk = true
		}
	}
	if !sawOk {
		t.Fatalf("expected raw tool result through when OnToolResult is nil — msgs: %+v", msgs)
	}
}
