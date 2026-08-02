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
//
// Every path reports what it rewrote through a ContextTraceEvent —
// including the unbudgeted default path, which used to prune in complete
// silence. The event is suppressed when nothing changed, so a run whose
// context always fits still emits nothing.
func (al *AgentLoop) enforceTokenBudget(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, iteration int, msgs []history.Message) []history.Message {
	orig := msgs
	if al.MaxTokenBudget <= 0 {
		pruned, changes := pruneContextMessages(msgs, defaultProtectedEnds)
		al.emitContextTrace(ctx, sessionKey, streamChan, iteration, ContextPolicyDefault, orig, pruned, changes)
		return pruned
	}

	estToks := estimateTokens(msgs)
	thresh := int(float64(al.MaxTokenBudget) * budgetWarnRatio)
	policy := ContextPolicyDefault
	var changes []ContextRef

	if estToks > thresh && estToks <= al.MaxTokenBudget {
		al.emit(ctx, sessionKey, streamChan, Event(ThoughtEvent{Message: fmt.Sprintf("Token budget near threshold (~%d >= %d). Truncating tool arguments.", estToks, thresh)}))
		var truncated []ContextRef
		msgs, truncated = truncateToolArguments(msgs)
		changes = append(changes, truncated...)
		policy = ContextPolicyBudgetWarn
	}

	depth := defaultProtectedEnds
	if postTrim := estimateTokens(msgs); postTrim > al.MaxTokenBudget {
		al.emit(ctx, sessionKey, streamChan, Event(ThoughtEvent{
			Message: fmt.Sprintf("Token budget exceeded (~%d est. tokens). Applying emergency context pruning.", postTrim),
		}))
		depth = emergencyProtectedEnds
		policy = ContextPolicyBudgetEmergency
	}

	pruned, prunedRefs := pruneContextMessages(msgs, depth)
	changes = append(changes, prunedRefs...)
	al.emitContextTrace(ctx, sessionKey, streamChan, iteration, policy, orig, pruned, changes)
	return pruned
}

// emitSoftLandingNudge fires the user-visible thought event marking the
// soft-landing window. The matching system-prompt augmentation lives in
// withSoftLandingHint; this is purely the telemetry signal.
func (al *AgentLoop) emitSoftLandingNudge(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, iteration int) {
	if iteration != al.MaxIters-softLandingMargin {
		return
	}
	al.emit(ctx, sessionKey, streamChan, Event(ThoughtEvent{Message: "System: Nudging Agent for a soft landing before MaxIters limit is reached."}))
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
	al.emit(ctx, sessionKey, streamChan, Event(ThoughtEvent{
		Message: fmt.Sprintf("Tool-call budget exceeded: executing first %d of %d; dropping %d.", al.MaxToolCallsPerTurn, len(calls), len(dropped)),
	}))
	return scheduled, dropped
}
