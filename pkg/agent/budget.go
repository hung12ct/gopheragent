package agent

import (
	"context"
	"fmt"
	"maps"
	"sync"
)

// BudgetTracker enforces per-session token budgets. It plugs into an AgentLoop
// in two places:
//
//   - Register Handler as an EventHandler to accumulate token usage from the
//     "usage" StreamEvent that providers emit after each LLM call.
//   - Register Guard as a BeforeLLMHook to deny new LLM calls once the running
//     total for a session exceeds the budget.
//
// The default behavior is to count TotalTokens against a single shared budget
// per session. Supply a CostFunc to convert tokens into dollars (or any other
// currency) when you need finer-grained control.
//
// BudgetTracker is safe for concurrent use.
//
// Example:
//
//	bt := agent.NewBudgetTracker(100_000) // 100k tokens per session
//	loop.OnEvent(bt.Handler())
//	loop.BeforeLLMHooks = append(loop.BeforeLLMHooks, bt.Guard())
type BudgetTracker struct {
	// Budget is the maximum cumulative TotalTokens permitted per session.
	// A value of 0 disables enforcement (Guard always returns nil) but the
	// Handler still records usage so callers can inspect via Usage.
	Budget int

	mu    sync.RWMutex
	usage map[string]TokenUsage
}

// NewBudgetTracker returns a tracker enforcing a cumulative token ceiling per
// session. Pass 0 to record usage without denying calls.
func NewBudgetTracker(budget int) *BudgetTracker {
	return &BudgetTracker{
		Budget: budget,
		usage:  make(map[string]TokenUsage),
	}
}

// Snapshot returns a copy of the per-session token usage recorded by the
// tracker. The returned map is safe for the caller to read and mutate; it
// reflects the state at the moment of the call.
func (bt *BudgetTracker) Snapshot() map[string]TokenUsage {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	out := make(map[string]TokenUsage, len(bt.usage))
	maps.Copy(out, bt.usage)
	return out
}

// Handler returns an EventHandler that accumulates token usage for each
// "usage" StreamEvent. Register it with loop.OnEvent.
func (bt *BudgetTracker) Handler() EventHandler {
	return func(_ context.Context, sessionKey string, ev StreamEvent) {
		p, ok := ev.Payload.(UsageEvent)
		if !ok {
			return
		}
		delta := p.Usage
		bt.mu.Lock()
		cur := bt.usage[sessionKey]
		cur.PromptTokens += delta.PromptTokens
		cur.CompletionTokens += delta.CompletionTokens
		cur.TotalTokens += delta.TotalTokens
		bt.usage[sessionKey] = cur
		bt.mu.Unlock()
	}
}

// Guard returns a BeforeLLMHook that denies the next LLM call once the
// session's cumulative TotalTokens exceeds Budget. Append it to
// loop.BeforeLLMHooks.
func (bt *BudgetTracker) Guard() BeforeLLMHook {
	return func(_ context.Context, sessionKey string, _ int) error {
		if bt.Budget <= 0 {
			return nil
		}
		bt.mu.RLock()
		spent := bt.usage[sessionKey].TotalTokens
		bt.mu.RUnlock()
		if spent >= bt.Budget {
			return fmt.Errorf("agent: token budget exhausted for session %q (spent %d of %d)", sessionKey, spent, bt.Budget)
		}
		return nil
	}
}

// Usage returns a snapshot of the recorded token usage for a session.
// Returns the zero value when the session has no usage yet.
func (bt *BudgetTracker) Usage(sessionKey string) TokenUsage {
	bt.mu.RLock()
	defer bt.mu.RUnlock()
	return bt.usage[sessionKey]
}

// Reset clears the recorded usage for a session, allowing it to start fresh
// against the budget. Reset with an empty string clears every session.
func (bt *BudgetTracker) Reset(sessionKey string) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if sessionKey == "" {
		bt.usage = make(map[string]TokenUsage)
		return
	}
	delete(bt.usage, sessionKey)
}

// Rewind subtracts the given TokenUsage from a session's accumulated total,
// effectively refunding tokens that were charged but won't be billed.
//
// Use cases:
//   - The user cancels a turn that was already mid-stream; the tokens that
//     reached the provider get charged but the partial output is discarded.
//     Refund so the user isn't double-billed against their cap.
//   - Regenerate replaces a prior answer; the old answer's tokens stop
//     counting against the live budget once the new turn lands.
//   - Sub-agent worker failed cleanly and the parent decides not to retry;
//     refund so the parent's session budget doesn't burn capacity on a
//     dropped result.
//
// Floors at zero per field so a buggy caller can't push the counter negative
// (which would then under-count future legitimate usage). Refunding a session
// that has no recorded usage is a no-op.
//
// Safe for concurrent use.
func (bt *BudgetTracker) Rewind(sessionKey string, refund TokenUsage) {
	if sessionKey == "" {
		return
	}
	bt.mu.Lock()
	defer bt.mu.Unlock()
	cur, ok := bt.usage[sessionKey]
	if !ok {
		return
	}
	cur.PromptTokens = subFloorZero(cur.PromptTokens, refund.PromptTokens)
	cur.CompletionTokens = subFloorZero(cur.CompletionTokens, refund.CompletionTokens)
	cur.TotalTokens = subFloorZero(cur.TotalTokens, refund.TotalTokens)
	bt.usage[sessionKey] = cur
}

// subFloorZero returns max(a-b, 0). Inlined helper so the three-field
// Rewind body stays readable.
func subFloorZero(a, b int) int {
	if b >= a {
		return 0
	}
	return a - b
}
