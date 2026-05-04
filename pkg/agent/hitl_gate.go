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
