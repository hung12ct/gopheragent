package agent

import (
	"context"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// ExitPlanModeToolName is the canonical name the AgentLoop special-cases
// when PlanMode is active. Registering a built-in tool (or custom tool)
// under this name is what lets the model signal "plan is ready".
const ExitPlanModeToolName = "exit_plan_mode"

// planModeSentinel identifies the auto-injected plan-mode hint so we do not
// append it twice.
const planModeSentinel = "PLAN MODE:"

// planModeHint is injected into the system prompt while PlanMode is true.
//
// Format discipline matters: the plan text the model returns drives the
// downstream UI (e.g. demo's Task Plan panel parses bullets into a
// checklist). A loose "produce a plan" prompt tends to yield 10+ items of
// varying granularity; the explicit budget below keeps plans actionable.
const planModeHint = planModeSentinel + " Do not call any tool except exit_plan_mode. " +
	"Produce a tight checklist of 3-7 top-level markdown bullets, each one a single concrete action in imperative form (e.g. \"- Search for benchmark data\", not \"- Investigate the topic\"). " +
	"Avoid sub-bullets unless one parent step has 2-4 essential sub-actions. No prose preamble, no closing summary — just the bullet list. " +
	"When the plan is complete, call exit_plan_mode with the plan as the `plan` argument. " +
	"The user must approve the plan before any tool runs; if they deny, you will receive their feedback and must revise."

// ConfirmPlanFunc receives the assistant's proposed plan text and returns
// true to approve (loop exits plan mode and resumes normal execution) or
// false to deny (the model is told to revise via the tool result).
type ConfirmPlanFunc func(ctx context.Context, plan string) bool

// planModeTool is the tool definition injected into the LLM's tool list
// when PlanMode is active. It is never executed — the loop intercepts calls
// to exit_plan_mode before they reach the registry — but the LLM must see
// its schema to be able to call it.
type planModeTool struct{}

type planModeToolArgs struct {
	Plan string `json:"plan" description:"The full plan as markdown text: goals, ordered steps, tools you will call, and acceptance criteria."`
}

const planModeToolDescription = "Propose a completed plan for the user's approval. Call this exactly once — when your plan is fully specified — with the plan text as `plan`. The system pauses execution until the user approves; approval unlocks normal tool use."

func (t *planModeTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        ExitPlanModeToolName,
		Description: planModeToolDescription,
		Parameters:  tools.SchemaFor[planModeToolArgs](),
		Display:     tools.DefaultDisplay(ExitPlanModeToolName, planModeToolDescription),
	}
}

func (t *planModeTool) Execute(_ context.Context, _ string) (tools.Result, error) {
	return tools.Text(`{"approved":true}`), nil
}

// withPlanModeTool returns a registry that includes exit_plan_mode,
// cloning the original only if the tool is not already registered.
func withPlanModeTool(reg *tools.Registry) *tools.Registry {
	if _, ok := reg.Get(ExitPlanModeToolName); ok {
		return reg
	}
	clone := reg.Clone()
	clone.Register(&planModeTool{})
	return clone
}

// withPlanModeHint returns msgs augmented with the plan-mode instructions
// when sessionKey is in plan mode. Input slice is never mutated; sentinel
// check ensures idempotency across retries.
func (al *AgentLoop) withPlanModeHint(sessionKey string, msgs []history.Message) []history.Message {
	if !al.IsPlanMode(sessionKey) {
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
