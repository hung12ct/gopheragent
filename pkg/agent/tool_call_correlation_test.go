package agent

import (
	"context"
	"sync"
	"testing"
)

// TestToolCall_PersistedRowCarriesCorrelationID asserts the live
// ToolCallEvent.ID (the opaque per-dispatch correlation ID) is persisted on
// the tool-result row as CorrelationID, so a consumer rendering an inline
// artifact card can re-match it after a session reload. Before this fix the
// live event carried newToolCallID() while the persisted row carried only
// the provider tool-call ID (tCall.ID) — two disjoint namespaces with no
// bridge, so reload-stable cards were impossible.
func TestToolCall_PersistedRowCarriesCorrelationID(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "plain", ArgsJSON: "{}"}}},
		{Content: "final"},
	}}
	loop, sm := setup(provider, plainTool{})

	var mu sync.Mutex
	var liveID string
	loop.EventHandlers = append(loop.EventHandlers, func(_ context.Context, _ string, ev StreamEvent) {
		if tc, ok := ev.Payload.(ToolCallEvent); ok {
			mu.Lock()
			liveID = tc.ID
			mu.Unlock()
		}
	})

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	gotLive := liveID
	mu.Unlock()
	if gotLive == "" {
		t.Fatal("no ToolCallEvent captured during the turn")
	}

	stored, _ := sm.History(context.Background(), "s1")
	var found bool
	for _, m := range stored {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			if m.CorrelationID != gotLive {
				t.Fatalf("tool row CorrelationID=%q, want live ToolCallEvent.ID %q", m.CorrelationID, gotLive)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no tool row recorded — stored=%+v", stored)
	}
}
