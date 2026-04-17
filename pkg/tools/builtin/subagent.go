package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// SubAgentInput is the JSON-decoded argument schema for CallSubAgentTool.
type SubAgentInput struct {
	TaskDescription string `json:"task_description"`
	AgentName       string `json:"agent_name"`
}

// CallSubAgentTool delegates a task to an isolated worker sub-agent with its own
// session state, preventing context-window pollution in the main agent.
type CallSubAgentTool struct {
	Sessions agent.SessionManager
	Tools    *tools.Registry
	LLM      agent.LLMProvider
}

// NewCallSubAgentTool constructs a CallSubAgentTool wired to the given dependencies.
func NewCallSubAgentTool(sessions agent.SessionManager, registry *tools.Registry, llm agent.LLMProvider) *CallSubAgentTool {
	return &CallSubAgentTool{
		Sessions: sessions,
		Tools:    registry,
		LLM:      llm,
	}
}

func (t *CallSubAgentTool) Name() string { return "call_sub_agent" }
func (t *CallSubAgentTool) Description() string {
	return "Delegates a complex reasoning or multi-step task to a specialized worker sub-agent. The sub-agent operates in a completely isolated context and returns a final structured report. Useful to avoid context window explosions."
}
func (t *CallSubAgentTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]any{
			"task_description": map[string]any{
				"type":        "string",
				"description": "Detailed instructions for the worker sub-agent.",
			},
			"agent_name": map[string]any{
				"type":        "string",
				"description": "Name of the worker persona (used to namespace the worker's session and tag its report).",
			},
		},
		Required: []string{"task_description", "agent_name"},
	}
}
func (t *CallSubAgentTool) RequiresConfirmation() bool { return false }

// Execute runs the sub-agent in an isolated session and returns its final report.
// The worker session is always deleted before returning, regardless of outcome,
// so no orphan history remains in the backing SessionManager.
func (t *CallSubAgentTool) Execute(ctx context.Context, inputJSON string) (string, error) {
	var input SubAgentInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("tools: invalid json: %w", err)
	}

	workerSessionKey := fmt.Sprintf("subagent-%s-%d", input.AgentName, time.Now().UnixNano())
	defer func() {
		// Always clean up the worker's ephemeral session; ignore error —
		// the tool's primary result is more important than cleanup failure.
		_ = t.Sessions.DeleteSession(context.Background(), workerSessionKey)
	}()

	workerLoop := agent.NewAgentLoop(t.Sessions, t.Tools, t.LLM)
	workerLoop.MaxIters = 15
	workerLoop.EmitThoughts = false

	instruction := fmt.Sprintf("[%s Sub-Agent Task]\n%s", input.AgentName, input.TaskDescription)

	result, err := workerLoop.RunIteration(ctx, workerSessionKey, instruction)
	if err != nil {
		return "", fmt.Errorf("tools: sub-agent %s failed: %w", input.AgentName, err)
	}

	return fmt.Sprintf("Report from %s:\n%s", input.AgentName, result), nil
}
