package agent

import (
	"context"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// ExitPlanModeToolName is the canonical name the AgentLoop special-cases
// when PlanMode is active. Registering a built-in tool (or custom tool)
// under this name is what lets the model signal "plan is ready".
const ExitPlanModeToolName = "exit_plan_mode"

// planModeSentinel identifies the auto-injected plan-mode hint so we do not
// append it twice.
const planModeSentinel = "PLAN MODE:"

// planModeHint is injected into the system prompt while PlanMode is true.
const planModeHint = planModeSentinel + " Do not call any tool except exit_plan_mode. " +
	"Think through the user's request and produce a concrete plan (goals, ordered steps, tools you will call, acceptance criteria). " +
	"When the plan is complete, call exit_plan_mode with the plan as the `plan` argument. " +
	"The user must approve the plan before any tool runs; if they deny, you will receive their feedback and must revise."

// ConfirmPlanFunc receives the assistant's proposed plan text and returns
// true to approve (loop exits plan mode and resumes normal execution) or
// false to deny (the model is told to revise via the tool result).
type ConfirmPlanFunc func(ctx context.Context, plan string) bool

// withPlanModeHint returns msgs augmented with the plan-mode instructions
// when PlanMode is active. Input slice is never mutated; sentinel check
// ensures idempotency across retries.
func (al *AgentLoop) withPlanModeHint(msgs []history.Message) []history.Message {
	if !al.PlanMode {
		return msgs
	}
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, planModeSentinel) {
			return msgs
		}
	}
	out := make([]history.Message, len(msgs))
	copy(out, msgs)
	if len(out) > 0 && out[0].Role == "system" {
		out[0].Content = out[0].Content + "\n\n" + planModeHint
		return out
	}
	return append([]history.Message{{Role: "system", Content: planModeHint}}, out...)
}
