package builtin

import (
	"context"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// RegisterExitPlanMode registers the plan-mode-exit sentinel tool. The
// AgentLoop intercepts calls to this tool by name while PlanMode is
// active and routes the plan through ConfirmPlan; the Execute body
// below is only reached when the loop is NOT in plan mode (e.g. the
// model called it unnecessarily) and returns a harmless
// acknowledgement so the run can continue.
//
// Registration pattern uses tools.RegisterFunc for the typed-args
// path; the tool itself carries no state.
func RegisterExitPlanMode(reg tools.Registerer) {
	tools.RegisterFunc(reg, exitPlanModeName, exitPlanModeDescription,
		func(_ context.Context, _ exitPlanModeArgs) (tools.Result, error) {
			return tools.Text(`{"approved":true,"note":"plan mode was not active; proceed with the plan."}`), nil
		})
}

const exitPlanModeName = "exit_plan_mode"
const exitPlanModeDescription = "Propose a completed plan for the user's approval. Call this exactly once — when your plan is fully specified — with the plan text as `plan`. The system pauses execution until the user approves; approval unlocks normal tool use."

type exitPlanModeArgs struct {
	Plan string `json:"plan" description:"The full plan as markdown text: goals, ordered steps, tools you will call, and acceptance criteria."`
}
