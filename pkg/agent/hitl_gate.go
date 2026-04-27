package agent

import (
	"context"
	"encoding/json"
)

// runHITLGate emits the human-confirmation prompt under hitlMu and
// returns the approval bool. Pure: caller owns the post-decision
// tool-message write so the gate stays composable.
//
// hitlMu is acquired and released within this method, matching the
// IIFE's defer-release semantics. The method must not be called while
// hitlMu is already held — runPlanModeGate acquires-and-releases on its
// own; the gates are independent locks-and-unlocks across the gap.
func (al *AgentLoop) runHITLGate(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, ws *waveState, tc PendingToolCall) bool {
	ws.hitlMu.Lock()
	defer ws.hitlMu.Unlock()

	al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeThought, Content: "CRITICAL: Tool requires human confirmation."})

	if al.ConfirmHITL != nil {
		return al.ConfirmHITL(ctx, tc.Name, tc.ArgsJSON)
	}
	payload, _ := json.Marshal(map[string]string{"tool": tc.Name, "args": tc.ArgsJSON})
	al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: EventTypeActionRequired, Content: string(payload)})
	return false
}
