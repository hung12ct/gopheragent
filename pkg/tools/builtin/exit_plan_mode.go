package builtin

import (
	"context"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// ExitPlanModeTool is the sentinel tool the LLM calls to signal "plan is
// ready — please ask the user to approve." The AgentLoop intercepts calls
// to this tool by name while PlanMode is active and routes the plan
// through ConfirmPlan; this Execute body is only reached when the loop is
// NOT in plan mode (e.g. the model called it unnecessarily) and returns a
// harmless acknowledgement so the run can continue.
type ExitPlanModeTool struct{}

// NewExitPlanModeTool returns a ready-to-register plan-mode exit tool.
func NewExitPlanModeTool() *ExitPlanModeTool { return &ExitPlanModeTool{} }

const exitPlanModeName = "exit_plan_mode"
const exitPlanModeDescription = "Propose a completed plan for the user's approval. Call this exactly once — when your plan is fully specified — with the plan text as `plan`. The system pauses execution until the user approves; approval unlocks normal tool use."

type exitPlanModeArgs struct {
	Plan string `json:"plan" description:"The full plan as markdown text: goals, ordered steps, tools you will call, and acceptance criteria."`
}

func (t *ExitPlanModeTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        exitPlanModeName,
		Description: exitPlanModeDescription,
		Parameters:  tools.SchemaFor[exitPlanModeArgs](),
		Display:     tools.DefaultDisplay(exitPlanModeName, exitPlanModeDescription),
	}
}

func (t *ExitPlanModeTool) Execute(_ context.Context, _ string) (tools.Result, error) {
	return tools.Text(`{"approved":true,"note":"plan mode was not active; proceed with the plan."}`), nil
}
