package agent

import (
	"context"
	"log"
	"time"
)

// hitlOutcome captures what happened at the HITL gate. The loop uses it to
// pick a directive matching the actual cause (user denial, timeout,
// configuration gap) instead of feeding the model a generic "denied"
// message that would mislead it into apologising for a non-existent
// rejection or routing around a gate the operator never saw.
type hitlOutcome int

const (
	hitlApproved hitlOutcome = iota
	hitlDenied
	hitlTimedOut
	hitlUnconfigured
)

// runHITLGate emits the human-confirmation prompt under hitlMu and returns
// a hitlOutcome describing how the gate resolved. The caller selects the
// tool-message directive from the outcome:
//   - hitlApproved   — proceed with execution.
//   - hitlDenied     — operator returned false within the configured window.
//   - hitlTimedOut   — AgentLoop.ConfirmHITLTimeout expired before a response.
//   - hitlUnconfigured — ConfirmHITL is nil; gate denied as a fallback.
//
// On non-approval the gate also emits a typed observational event
// (EventTypeHITLDenied / EventTypeHITLTimedOut) so sub-agent wrappers and
// UI layers can route on the cause without scraping tool-message text.
//
// hitlMu is acquired and released within this method, matching the
// IIFE's defer-release semantics. The method must not be called while
// hitlMu is already held — runPlanModeGate acquires-and-releases on its
// own; the gates are independent locks-and-unlocks across the gap.
func (al *AgentLoop) runHITLGate(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, ws *waveState, tc PendingToolCall) hitlOutcome {
	ws.hitlMu.Lock()
	defer ws.hitlMu.Unlock()

	al.emit(ctx, sessionKey, streamChan, Event(ThoughtEvent{Message: "CRITICAL: Tool requires human confirmation."}))

	if al.ConfirmHITL == nil {
		al.warnMissingConfirmHITLOnce()
		al.emit(ctx, sessionKey, streamChan, Event(ActionRequiredEvent{Tool: tc.Name, Args: tc.ArgsJSON}))
		return hitlUnconfigured
	}

	confirmCtx := ctx
	if al.ConfirmHITLTimeout > 0 {
		var cancel context.CancelFunc
		confirmCtx, cancel = context.WithTimeout(ctx, al.ConfirmHITLTimeout)
		defer cancel()
	}

	if al.ConfirmHITL(confirmCtx, tc.Name, tc.ArgsJSON) {
		return hitlApproved
	}

	// confirmCtx fired but the outer ctx did not → our timeout caused it,
	// not a parent cancellation propagated downward. Treat that as the
	// timeout outcome so the directive text matches what the operator saw.
	if al.ConfirmHITLTimeout > 0 && ctx.Err() == nil && confirmCtx.Err() != nil {
		al.emit(ctx, sessionKey, streamChan, Event(HITLTimedOutEvent{
			Tool:    tc.Name,
			Args:    tc.ArgsJSON,
			Timeout: al.ConfirmHITLTimeout,
		}))
		return hitlTimedOut
	}

	al.emit(ctx, sessionKey, streamChan, Event(HITLDeniedEvent{Tool: tc.Name, Args: tc.ArgsJSON}))
	return hitlDenied
}

// warnMissingConfirmHITLOnce logs a one-time warning the first time a
// confirmation gate fires without a ConfirmHITL callback. Tied to a
// loop-scoped sync.Once so each AgentLoop instance can warn
// independently without spamming on every tool call.
func (al *AgentLoop) warnMissingConfirmHITLOnce() {
	al.confirmHITLWarnOnce.Do(func() {
		log.Printf("[agent] WARNING: a tool reached the HITL gate but AgentLoop.ConfirmHITL is nil; the gate will deny silently unless an EventTypeActionRequired listener resolves it out of band. Wire ConfirmHITL or a PermissionRuleSet covering tools that declare RequiresConfirmation()=true.")
	})
}

// confirmHITLKey is the unexported context key used to ferry the parent
// loop's ConfirmFunc down to sub-agent tools. Mirrors the dynamic-context
// propagation pattern so sub-agents (CallSQLAgentTool, CallSubAgentTool)
// can wire the parent's HITL gate onto their worker loop without the
// adopter having to plumb it through manually.
type confirmHITLKey struct{}

// WithConfirmHITL returns a derived ctx that carries fn so a downstream
// sub-agent tool can recover it via ConfirmHITLFromContext and assign it
// onto its worker AgentLoop.ConfirmHITL. A nil fn returns ctx unchanged,
// which is zero-cost when the parent loop has no ConfirmHITL configured.
func WithConfirmHITL(ctx context.Context, fn ConfirmFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, confirmHITLKey{}, fn)
}

// ConfirmHITLFromContext returns the ConfirmFunc stamped onto ctx by the
// parent loop, or nil if none is present. Sub-agent tools call this
// immediately after constructing their worker AgentLoop and assign the
// result so RequiresConfirmation tools inside the sub-agent surface
// through the same approval path as the outer loop.
func ConfirmHITLFromContext(ctx context.Context) ConfirmFunc {
	if v, ok := ctx.Value(confirmHITLKey{}).(ConfirmFunc); ok {
		return v
	}
	return nil
}

// confirmHITLTimeoutKey is the unexported context key for propagating the
// parent loop's HITL approval window down to sub-agent worker loops.
type confirmHITLTimeoutKey struct{}

// WithConfirmHITLTimeout returns a derived ctx that carries d so a downstream
// sub-agent tool can recover it via ConfirmHITLTimeoutFromContext and assign
// it onto its worker AgentLoop.ConfirmHITLTimeout. A zero d returns ctx
// unchanged so the helper stays zero-cost when the parent has not opted in
// to a HITL timeout.
func WithConfirmHITLTimeout(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, confirmHITLTimeoutKey{}, d)
}

// ConfirmHITLTimeoutFromContext returns the timeout stamped onto ctx by the
// parent loop, or 0 if none is present. Sub-agent tools that recover this
// alongside ConfirmHITLFromContext keep the operator's approval window
// consistent whether the gate fires at the outer boundary (call_sql_agent)
// or inside the inner worker (execute_sql).
func ConfirmHITLTimeoutFromContext(ctx context.Context) time.Duration {
	if v, ok := ctx.Value(confirmHITLTimeoutKey{}).(time.Duration); ok {
		return v
	}
	return 0
}
