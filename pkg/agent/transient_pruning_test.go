package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// TestPruning_DoesNotPersistTrimmedMessages locks in the contract: the
// runIteration token-budget path produces a transient pruned slice for
// the LLM call only. The session history written via SetHistory must
// retain the original full-fidelity tool output.
func TestPruning_DoesNotPersistTrimmedMessages(t *testing.T) {
	bigContent := strings.Repeat("X", SoftTrimThreshold*2) // exceeds soft-trim threshold

	provider := &scriptProvider{turns: []LLMResult{
		{Content: "ok"},
	}}
	loop, sm := setup(provider)

	// Seed a session with a long tool message at index 1 so that, after
	// RunIteration appends the new user message, the tool sits OUTSIDE
	// PruneContextMessages's protected-tail window (last 3 messages) and
	// becomes a soft-trim candidate.
	seed := []history.Message{
		{Role: "system", Content: "be helpful"},
		{Role: "tool", ToolCallID: "c1", Content: bigContent},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ok"},
	}
	sm.SetHistory(context.Background(), "s1", seed)

	if _, err := loop.RunIteration(context.Background(), "s1", "next turn"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stored := sm.GetHistory(context.Background(), "s1")
	var foundFull bool
	for _, m := range stored {
		if m.Role == "tool" && m.ToolCallID == "c1" && len(m.Content) >= len(bigContent) {
			foundFull = true
		}
	}
	if !foundFull {
		t.Fatalf("transient pruning leaked into persisted history — expected the full %d-rune tool message to survive in storage", len(bigContent))
	}
}
