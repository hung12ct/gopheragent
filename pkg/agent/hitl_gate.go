package agent

import (
	"context"
	"encoding/json"
	"log"
)

// runHITLGate emits the human-confirmation prompt under hitlMu and
// returns (approved, callbackWired). callbackWired is false when
// AgentLoop.ConfirmHITL is nil — the caller uses that flag to surface a
// "gate unconfigured" directive instead of a generic "user denied" one,
// which kept the model from confabulating user rejection in setups
// where no approval path was wired.
//
// hitlMu is acquired and released within this method, matching the
// IIFE's defer-release semantics. The method must not be called while
// hitlMu is already held — runPlanModeGate acquires-and-releases on its
// own; the gates are independent locks-and-unlocks across the gap.
func (al *AgentLoop) runHITLGate(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, ws *waveState, tc PendingToolCall) (approved bool, callbackWired bool) {
	ws.hitlMu.Lock()
	defer ws.hitlMu.Unlock()

	al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: "CRITICAL: Tool requires human confirmation."})

	if al.ConfirmHITL != nil {
		return al.ConfirmHITL(ctx, tc.Name, tc.ArgsJSON), true
	}
	al.warnMissingConfirmHITLOnce()
	payload, _ := json.Marshal(map[string]string{"tool": tc.Name, "args": tc.ArgsJSON})
	al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeActionRequired, Content: string(payload)})
	return false, false
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
