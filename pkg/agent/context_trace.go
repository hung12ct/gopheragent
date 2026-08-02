package agent

import (
	"context"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// ContextChangeReason classifies why the loop rewrote a message before
// handing the conversation to the provider. It is a closed enum — the
// pruner has exactly three ways to shrink a message.
type ContextChangeReason string

const (
	// ContextChangeSoftTrim: a long tool/assistant message had its middle
	// cut out by the depth-based prune; head and tail survive.
	ContextChangeSoftTrim ContextChangeReason = "soft-trim"
	// ContextChangeOutlierDiscarded: the message blew past the outlier
	// ceiling and its entire payload was replaced by a system notice. This
	// is the only reason that loses content outright.
	ContextChangeOutlierDiscarded ContextChangeReason = "outlier-discarded"
	// ContextChangeArgsTruncated: a tool result was clipped to its first
	// few hundred runes because the run crossed the token-budget warn
	// threshold.
	ContextChangeArgsTruncated ContextChangeReason = "args-truncated"
)

// ContextPolicy names which enforceTokenBudget path produced a trace, so a
// host can tell routine depth pruning apart from budget pressure.
type ContextPolicy string

const (
	// ContextPolicyDefault is the unbudgeted path — MaxTokenBudget is
	// unset, so only the standard depth prune ran.
	ContextPolicyDefault ContextPolicy = "default"
	// ContextPolicyBudgetWarn: the estimate crossed budgetWarnRatio of
	// MaxTokenBudget and tool arguments were truncated.
	ContextPolicyBudgetWarn ContextPolicy = "budget-warn"
	// ContextPolicyBudgetEmergency: the estimate exceeded MaxTokenBudget
	// outright and the aggressive shallow prune ran.
	ContextPolicyBudgetEmergency ContextPolicy = "budget-emergency"
)

// ContextRef identifies one message the pruner rewrote for a single LLM
// call. Index is the position in the pre-prune slice; because no pruning
// path reorders or removes messages, it also indexes the post-prune
// slice — and, since the loop prunes a transient copy and persists the
// conversation at full fidelity, the same position in the slice returned
// by Sessions.History. That is what makes a trace joinable back to the
// stored transcript.
//
// ToolCallID and CorrelationID are the stable handles for naming a tool
// result across events — CorrelationID is the agent-generated ID also
// carried by ToolCallEvent, so a host can join a trimmed message back to
// the call that produced it. Both are empty for assistant messages.
//
// EstTokensBefore/After use the same 4-chars/token heuristic as
// MaxTokenBudget enforcement, so they are comparable with the event's
// totals but are not exact provider counts.
type ContextRef struct {
	Index           int                 `json:"index"`
	Role            string              `json:"role"`
	ToolCallID      string              `json:"tool_call_id,omitempty"`
	CorrelationID   string              `json:"correlation_id,omitempty"`
	Reason          ContextChangeReason `json:"reason"`
	EstTokensBefore int                 `json:"est_tokens_before"`
	EstTokensAfter  int                 `json:"est_tokens_after"`
}

// contextRefFor builds a trace entry for msg at index i. estAfter is the
// post-rewrite estimate; the caller supplies it because only the rewriting
// branch knows the replacement content.
func contextRefFor(i int, msg history.Message, reason ContextChangeReason, estBefore, estAfter int) ContextRef {
	return ContextRef{
		Index:           i,
		Role:            msg.Role,
		ToolCallID:      msg.ToolCallID,
		CorrelationID:   msg.CorrelationID,
		Reason:          reason,
		EstTokensBefore: estBefore,
		EstTokensAfter:  estAfter,
	}
}

// emitContextTrace fires a ContextTraceEvent describing what the pruner
// changed on the way into one LLM call. No-op when nothing was rewritten,
// which is the common case — a turn whose messages all fit emits nothing,
// and the two estimateTokens sweeps only run when there is something to
// report.
func (al *AgentLoop) emitContextTrace(
	ctx context.Context,
	sessionKey string,
	streamChan chan<- StreamEvent,
	iteration int,
	policy ContextPolicy,
	before, after []history.Message,
	changes []ContextRef,
) {
	if len(changes) == 0 {
		return
	}
	al.emit(ctx, sessionKey, streamChan, Event(ContextTraceEvent{
		Policy:          policy,
		Iteration:       iteration,
		Changes:         changes,
		EstTokensBefore: estimateTokens(before),
		EstTokensAfter:  estimateTokens(after),
	}))
}
