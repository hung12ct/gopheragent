package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// asyncTaskSchema returns the standard schema for tools that accept only {task_id}.
func asyncTaskIDSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "The task_id returned by start_async_task.",
			},
		},
		Required: []string{"task_id"},
	}
}

// StartAsyncTaskTool spawns a background worker sub-agent and returns a task_id
// that can be polled, cancelled, or steered via the other async tools.
type StartAsyncTaskTool struct {
	Manager *agent.AsyncTaskManager
}

func (t *StartAsyncTaskTool) Name() string { return "start_async_task" }
func (t *StartAsyncTaskTool) Description() string {
	return "Starts a background worker sub-agent to handle a long-running task. Returns a task_id that can be used with check_async_task."
}
func (t *StartAsyncTaskTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]any{
			"task_description": map[string]any{
				"type":        "string",
				"description": "Detailed instructions for the background worker.",
			},
			"agent_name": map[string]any{
				"type":        "string",
				"description": "Name of the worker persona (used to tag the task).",
			},
		},
		Required: []string{"task_description", "agent_name"},
	}
}
func (t *StartAsyncTaskTool) RequiresConfirmation() bool { return false }
func (t *StartAsyncTaskTool) Execute(ctx context.Context, inputJSON string) (string, error) {
	sessionKey, ok := ctx.Value(agent.SessionKeyCtx("sessionKey")).(string)
	if !ok {
		return "", fmt.Errorf("tools: sessionKey not found in context")
	}
	var input struct {
		TaskDescription string `json:"task_description"`
		AgentName       string `json:"agent_name"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("tools: invalid json: %w", err)
	}
	taskID, err := t.Manager.StartTask(ctx, sessionKey, input.AgentName, input.TaskDescription)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Background task started. task_id: %s", taskID), nil
}

// CheckAsyncTaskTool reports the status or final result of an async task.
type CheckAsyncTaskTool struct {
	Manager *agent.AsyncTaskManager
}

func (t *CheckAsyncTaskTool) Name() string { return "check_async_task" }
func (t *CheckAsyncTaskTool) Description() string {
	return "Checks the status or retrieves the result of a background async task using its task_id."
}
func (t *CheckAsyncTaskTool) ParametersSchema() tools.ToolSchema { return asyncTaskIDSchema() }
func (t *CheckAsyncTaskTool) RequiresConfirmation() bool         { return false }
func (t *CheckAsyncTaskTool) Execute(ctx context.Context, inputJSON string) (string, error) {
	sessionKey, ok := ctx.Value(agent.SessionKeyCtx("sessionKey")).(string)
	if !ok {
		return "", fmt.Errorf("tools: sessionKey not found in context")
	}
	var input struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("tools: invalid json: %w", err)
	}

	tasks := t.Manager.SyncTasks(sessionKey)
	task, ok := tasks[input.TaskID]
	if !ok {
		return "", fmt.Errorf("tools: task_id %s not found", input.TaskID)
	}

	if task.Status == "running" {
		return fmt.Sprintf("Task %s is still running. Please check again later.", input.TaskID), nil
	}
	return fmt.Sprintf("Task %s completed with status: %s\nResult: %s", input.TaskID, task.Status, task.Result), nil
}

// CancelAsyncTaskTool signals a running async task to stop via its CancelFunc.
type CancelAsyncTaskTool struct {
	Manager *agent.AsyncTaskManager
}

func (t *CancelAsyncTaskTool) Name() string                        { return "cancel_async_task" }
func (t *CancelAsyncTaskTool) Description() string                 { return "Cancels a running background task." }
func (t *CancelAsyncTaskTool) ParametersSchema() tools.ToolSchema  { return asyncTaskIDSchema() }
func (t *CancelAsyncTaskTool) RequiresConfirmation() bool          { return false }
func (t *CancelAsyncTaskTool) Execute(ctx context.Context, inputJSON string) (string, error) {
	sessionKey, ok := ctx.Value(agent.SessionKeyCtx("sessionKey")).(string)
	if !ok {
		return "", fmt.Errorf("tools: sessionKey not found in context")
	}
	var input struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("tools: invalid json: %w", err)
	}

	if err := t.Manager.CancelTask(sessionKey, input.TaskID); err != nil {
		return "", err
	}
	return fmt.Sprintf("Task %s has been cancelled.", input.TaskID), nil
}

// UpdateAsyncTaskTool delivers a steering instruction to a running worker.
type UpdateAsyncTaskTool struct {
	Manager *agent.AsyncTaskManager
}

func (t *UpdateAsyncTaskTool) Name() string { return "update_async_task" }
func (t *UpdateAsyncTaskTool) Description() string {
	return "Sends additional instructions or steers a running background task."
}
func (t *UpdateAsyncTaskTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "The task_id returned by start_async_task.",
			},
			"instruction": map[string]any{
				"type":        "string",
				"description": "Additional directive delivered to the running worker before its next iteration.",
			},
		},
		Required: []string{"task_id", "instruction"},
	}
}
func (t *UpdateAsyncTaskTool) RequiresConfirmation() bool { return false }
func (t *UpdateAsyncTaskTool) Execute(ctx context.Context, inputJSON string) (string, error) {
	var input struct {
		TaskID      string `json:"task_id"`
		Instruction string `json:"instruction"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("tools: invalid json: %w", err)
	}

	if err := t.Manager.UpdateTask(input.TaskID, input.Instruction); err != nil {
		return "", err
	}
	return fmt.Sprintf("Instruction sent to task %s.", input.TaskID), nil
}

// ListAsyncTasksTool enumerates every async task tracked in the current session.
type ListAsyncTasksTool struct {
	Manager *agent.AsyncTaskManager
}

func (t *ListAsyncTasksTool) Name() string                       { return "list_async_tasks" }
func (t *ListAsyncTasksTool) Description() string                { return "Lists all async tasks in the current session." }
func (t *ListAsyncTasksTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{Type: "object", Properties: map[string]any{}}
}
func (t *ListAsyncTasksTool) RequiresConfirmation() bool { return false }
func (t *ListAsyncTasksTool) Execute(ctx context.Context, inputJSON string) (string, error) {
	sessionKey, ok := ctx.Value(agent.SessionKeyCtx("sessionKey")).(string)
	if !ok {
		return "", fmt.Errorf("tools: sessionKey not found in context")
	}
	tasks := t.Manager.SyncTasks(sessionKey)
	if len(tasks) == 0 {
		return "No async tasks found in this session.", nil
	}

	var sb strings.Builder
	for _, task := range tasks {
		sb.WriteString(fmt.Sprintf("- %s (Agent: %s, Status: %s)\n", task.TaskID, task.AgentName, task.Status))
	}
	return sb.String(), nil
}
