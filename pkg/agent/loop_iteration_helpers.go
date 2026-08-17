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
	d := deriveRequestMessages(msgs, al.MaxTokenBudget)
	for _, notice := range d.notices {
		al.emit(ctx, sessionKey, streamChan, Event(ThoughtEvent{Message: notice}))
	}
	al.emitContextTrace(ctx, sessionKey, streamChan, iteration, d.policy, msgs, d.messages, d.changes)
	return d.messages
}

// contextDerivation is one run of the budget policy over stored history:
// the derived message list plus everything the emitting half needs to
// report what it did.
type contextDerivation struct {
	messages []history.Message
	policy   ContextPolicy
	changes  []ContextRef
	notices  []string
}

// deriveRequestMessages turns stored history into the message list a single
// LLM call should carry. It is the pure half of enforceTokenBudget: same
// input, same output, no emit, no receiver state. Purity is what lets the
// request invariant re-run it as an independent check — see
// AgentLoop.checkRequestInvariant.
func deriveRequestMessages(stored []history.Message, maxTokenBudget int) contextDerivation {
	if maxTokenBudget <= 0 {
		pruned, changes := pruneContextMessages(stored, defaultProtectedEnds)
		return contextDerivation{messages: pruned, policy: ContextPolicyDefault, changes: changes}
	}

	msgs := stored
	estToks := estimateTokens(msgs)
	thresh := int(float64(maxTokenBudget) * budgetWarnRatio)
	d := contextDerivation{policy: ContextPolicyDefault}

	if estToks > thresh && estToks <= maxTokenBudget {
		d.notices = append(d.notices, fmt.Sprintf("Token budget near threshold (~%d >= %d). Truncating tool arguments.", estToks, thresh))
		var truncated []ContextRef
		msgs, truncated = truncateToolArguments(msgs)
		d.changes = append(d.changes, truncated...)
		d.policy = ContextPolicyBudgetWarn
	}

	depth := defaultProtectedEnds
	if postTrim := estimateTokens(msgs); postTrim > maxTokenBudget {
		d.notices = append(d.notices, fmt.Sprintf("Token budget exceeded (~%d est. tokens). Applying emergency context pruning.", postTrim))
		depth = emergencyProtectedEnds
		d.policy = ContextPolicyBudgetEmergency
	}

	pruned, prunedRefs := pruneContextMessages(msgs, depth)
	d.messages = pruned
	d.changes = append(d.changes, prunedRefs...)
	return d
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
