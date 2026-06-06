package agent

import (
	"context"
	"encoding/json"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// runPlanModeGate intercepts a pending tool call before registry lookup
// when plan mode is active. Returns handled=true when the gate has
// recorded a tool result for the call (the caller must `return` from
// the per-tool goroutine immediately); handled=false means the call
// should proceed through the normal execution path.
//
// Locks ws.hitlMu for the full duration so the plan-approval prompt
// and the HITL prompt cannot interleave across parallel goroutines —
// matching the contract of the original IIFE.
func (al *AgentLoop) runPlanModeGate(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, ws *waveState, tc PendingToolCall) bool {
	ws.hitlMu.Lock()
	defer ws.hitlMu.Unlock()
	if !al.IsPlanMode(sessionKey) {
		return false
	}
	if tc.Name != ExitPlanModeToolName {
		ws.recordToolMsg(tc.ID, history.Message{
			Role:       "tool",
			Content:    planGateBlockedMessage(tc.Name),
			ToolCallID: tc.ID,
			IsError:    true,
		}, false)
		return true
	}
	var pa struct {
		Plan string `json:"plan"`
	}
	_ = json.Unmarshal([]byte(tc.ArgsJSON), &pa)
	al.emit(ctx, sessionKey, streamChan, Event(ThoughtEvent{Message: "Plan proposed — awaiting human approval."}))
	approved := false
	if al.ConfirmPlan != nil {
		approved = al.ConfirmPlan(ctx, PlanProposal{Plan: pa.Plan, RawArgs: json.RawMessage(tc.ArgsJSON)})
	} else {
		al.emit(ctx, sessionKey, streamChan, Event(ActionRequiredEvent{Tool: ExitPlanModeToolName, Args: pa.Plan}))
	}
	var result string
	isErr := false
	if approved {
		al.SetPlanMode(sessionKey, false)
		al.emit(ctx, sessionKey, streamChan, Event(ThoughtEvent{Message: "Plan approved — exiting plan mode."}))
		result = planApprovedJSON()
	} else {
		result = planDeniedJSON()
		isErr = true
	}
	ws.recordToolMsg(tc.ID, history.Message{Role: "tool", Content: result, ToolCallID: tc.ID, IsError: isErr}, !isErr)
	return true
}
