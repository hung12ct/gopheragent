package agent

import (
	"context"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func TestNew_AppliesDefaults(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	provider := &scriptProvider{}
	loop := New(sm, reg, provider)

	if loop.MaxIters != 15 {
		t.Errorf("MaxIters default: got %d, want 15", loop.MaxIters)
	}
	if !loop.EmitThoughts {
		t.Error("EmitThoughts default: should be true")
	}
	if !loop.AutoCacheSystem {
		t.Error("AutoCacheSystem default: should be true")
	}
}

func TestNew_OptionsOverrideDefaults(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	provider := &scriptProvider{}

	confirmFn := func(_ context.Context, _, _ string) bool { return false }

	loop := New(sm, reg, provider,
		WithMaxIters(3),
		WithEmitThoughts(false),
		WithoutAutoCacheSystem(),
		WithHITL(confirmFn, 5*time.Second),
		WithMaxToolCallsPerTurn(8),
		WithMaxToolCallsPerSession(20),
		WithSpeculativeTools(true),
		WithReflect(2, "custom critique"),
		WithThinking(2048),
	)

	if loop.MaxIters != 3 {
		t.Errorf("MaxIters: got %d, want 3", loop.MaxIters)
	}
	if loop.EmitThoughts {
		t.Error("EmitThoughts: should be false")
	}
	if loop.AutoCacheSystem {
		t.Error("AutoCacheSystem: should be false")
	}
	if loop.ConfirmHITL == nil {
		t.Error("ConfirmHITL: should be set")
	}
	if loop.ConfirmHITLTimeout != 5*time.Second {
		t.Errorf("ConfirmHITLTimeout: got %v, want 5s", loop.ConfirmHITLTimeout)
	}
	if loop.MaxToolCallsPerTurn != 8 {
		t.Errorf("MaxToolCallsPerTurn: got %d, want 8", loop.MaxToolCallsPerTurn)
	}
	if loop.MaxToolCallsPerSession != 20 {
		t.Errorf("MaxToolCallsPerSession: got %d, want 20", loop.MaxToolCallsPerSession)
	}
	if !loop.SpeculativeTools {
		t.Error("SpeculativeTools: should be true")
	}
	if loop.Reflect != 2 {
		t.Errorf("Reflect: got %d, want 2", loop.Reflect)
	}
	if loop.ReflectPrompt != "custom critique" {
		t.Errorf("ReflectPrompt: got %q, want 'custom critique'", loop.ReflectPrompt)
	}
	if loop.ThinkingBudget != 2048 {
		t.Errorf("ThinkingBudget: got %d, want 2048", loop.ThinkingBudget)
	}
}

func TestNew_HookOptionsAppend(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	provider := &scriptProvider{}

	var bHook1, bHook2, llmHook, evHandler bool
	loop := New(sm, reg, provider,
		WithBeforeHook(func(_ context.Context, _, _ string) error { bHook1 = true; return nil }),
		WithBeforeHook(func(_ context.Context, _, _ string) error { bHook2 = true; return nil }),
		WithBeforeLLMHook(func(_ context.Context, _ string, _ int) error { llmHook = true; return nil }),
		WithOnEvent(func(_ context.Context, _ string, _ StreamEvent) { evHandler = true }),
	)

	if len(loop.BeforeHooks) != 2 {
		t.Fatalf("BeforeHooks: got %d, want 2", len(loop.BeforeHooks))
	}
	_ = loop.BeforeHooks[0](context.Background(), "s", "x")
	_ = loop.BeforeHooks[1](context.Background(), "s", "x")
	if !bHook1 || !bHook2 {
		t.Error("both BeforeHooks should fire in registration order")
	}

	if len(loop.BeforeLLMHooks) != 1 {
		t.Fatalf("BeforeLLMHooks: got %d, want 1", len(loop.BeforeLLMHooks))
	}
	_ = loop.BeforeLLMHooks[0](context.Background(), "s", 100)
	if !llmHook {
		t.Error("BeforeLLMHook should fire")
	}

	if len(loop.EventHandlers) != 1 {
		t.Fatalf("EventHandlers: got %d, want 1", len(loop.EventHandlers))
	}
	loop.EventHandlers[0](context.Background(), "s", Event(DoneEvent{}))
	if !evHandler {
		t.Error("EventHandler should fire")
	}
}

func TestNew_NilOptionsAreSkipped(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	provider := &scriptProvider{}
	// Caller may build the option slice conditionally; nil entries must not
	// panic. The for-range in New silently skips them.
	opts := []Option{nil, WithMaxIters(7), nil}
	loop := New(sm, reg, provider, opts...)
	if loop.MaxIters != 7 {
		t.Errorf("MaxIters: got %d, want 7 (nil options should skip without panic)", loop.MaxIters)
	}
}
