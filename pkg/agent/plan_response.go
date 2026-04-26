package agent

import (
	"encoding/json"
	"fmt"
)

// planGateBlockedMessage is the structured tool result the loop writes
// back when the model attempts a non-exit_plan_mode tool while plan mode
// is active. The JSON shape is what the LLM sees in its tool result;
// keeping it marshaled (not raw-string formatted) avoids escaping bugs
// when the tool name contains quotes or other JSON-meaningful characters.
func planGateBlockedMessage(toolName string) string {
	body, err := json.Marshal(struct {
		Error string `json:"error"`
	}{
		Error: fmt.Sprintf("tool %q is blocked in plan mode. Present your full plan via exit_plan_mode first and wait for user approval.", toolName),
	})
	if err != nil {
		return `{"error":"tool blocked in plan mode"}`
	}
	return string(body)
}

// planApprovalResult is the tool result the loop writes when the user
// has decided on an exit_plan_mode call.
type planApprovalResult struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
}

func planApprovedJSON() string {
	return marshalPlanResult(planApprovalResult{Approved: true})
}

func planDeniedJSON() string {
	return marshalPlanResult(planApprovalResult{
		Approved: false,
		Reason:   "User rejected the plan. Revise based on their feedback and propose again via exit_plan_mode.",
	})
}

func marshalPlanResult(r planApprovalResult) string {
	body, err := json.Marshal(r)
	if err != nil {
		return `{"approved":false}`
	}
	return string(body)
}
