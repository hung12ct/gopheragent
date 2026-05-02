package agent

import (
	"context"
	"fmt"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// enforceTokenBudget applies the configured MaxTokenBudget by trimming
// tool arguments at the warn threshold and aggressively pruning context
// when the absolute ceiling is exceeded. Returns a derived slice intended
// for the immediate LLM call — callers must NOT persist the result via
// SetHistory or saveSession; the input msgs slice is the source of truth
// for what gets stored. When MaxTokenBudget is zero, falls back to the
// standard PruneContextMessages with default depth.
func (al *AgentLoop) enforceTokenBudget(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, msgs []history.Message) []history.Message {
	if al.MaxTokenBudget <= 0 {
		return PruneContextMessages(msgs, 3)
	}

	estToks := estimateTokens(msgs)
	thresh := int(float64(al.MaxTokenBudget) * budgetWarnRatio)

	if estToks > thresh && estToks <= al.MaxTokenBudget {
		al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf("Token budget near threshold (~%d >= %d). Truncating tool arguments.", estToks, thresh)})
		msgs = TruncateToolArguments(msgs)
	}

	if estimateTokens(msgs) > al.MaxTokenBudget {
		al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf(
			"Token budget exceeded (~%d est. tokens). Applying emergency context pruning.", estimateTokens(msgs),
		)})
		return PruneContextMessages(msgs, 1)
	}
	return PruneContextMessages(msgs, 3)
}

// emitSoftLandingNudge fires the user-visible thought event marking the
// soft-landing window. The matching system-prompt augmentation lives in
// withSoftLandingHint; this is purely the telemetry signal.
func (al *AgentLoop) emitSoftLandingNudge(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, iteration int) {
	if iteration != al.MaxIters-softLandingMargin {
		return
	}
	al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: "System: Nudging Agent for a soft landing before MaxIters limit is reached."})
}

// applyToolCallBudget splits the LLM's tool-call list into the slice
// that will execute and the slice dropped by the per-turn cap. Emits a
// thought event when truncation happens. When MaxToolCallsPerTurn is 0,
// scheduled is the full list and dropped is nil.
func (al *AgentLoop) applyToolCallBudget(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, calls []PendingToolCall) (scheduled, dropped []PendingToolCall) {
	if al.MaxToolCallsPerTurn <= 0 || len(calls) <= al.MaxToolCallsPerTurn {
		return calls, nil
	}
	dropped = calls[al.MaxToolCallsPerTurn:]
	scheduled = calls[:al.MaxToolCallsPerTurn]
	al.emit(ctx, sessionKey, streamChan, StreamEvent{
		Type:    "thought",
		Content: fmt.Sprintf("Tool-call budget exceeded: executing first %d of %d; dropping %d.", al.MaxToolCallsPerTurn, len(calls), len(dropped)),
	})
	return scheduled, dropped
}
