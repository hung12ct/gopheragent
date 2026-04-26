package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// TestEmit_HandlerPanicRecovered pins that a panicking event handler is
// isolated by safeCallHandler — the loop completes normally, subsequent
// handlers still run, and the user-facing answer is unaffected.
func TestEmit_HandlerPanicRecovered(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{Content: "Hello"},
	}}
	loop, _ := setup(provider)

	var afterPanicHits int32
	loop.OnEvent(func(_ context.Context, _ string, ev StreamEvent) {
		if ev.Type == EventTypeContent {
			panic("synthetic handler crash")
		}
	})
	// Second handler must still fire even though the first one panicked
	// for the same event — handlers are independent.
	loop.OnEvent(func(_ context.Context, _ string, ev StreamEvent) {
		if ev.Type == EventTypeContent {
			atomic.AddInt32(&afterPanicHits, 1)
		}
	})

	resp, err := loop.RunIteration(context.Background(), "s1", "go")
	if err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if !strings.Contains(resp, "Hello") {
		t.Fatalf("loop should have completed despite panic: %q", resp)
	}
	if atomic.LoadInt32(&afterPanicHits) == 0 {
		t.Fatal("second handler must still fire after first one panics")
	}
}
