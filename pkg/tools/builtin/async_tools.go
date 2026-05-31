package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// asyncTaskIDSchema returns the standard schema for tools that accept only {task_id}.
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
// Construct via NewStartAsyncTaskTool — the manager is required.
type StartAsyncTaskTool struct {
	manager *agent.AsyncTaskManager
}

// NewStartAsyncTaskTool returns a StartAsyncTaskTool bound to mgr. Panics if
// mgr is nil — every async tool needs a manager and a nil panic at startup is
// strictly better than a nil-pointer deref on the first Execute.
func NewStartAsyncTaskTool(mgr *agent.AsyncTaskManager) *StartAsyncTaskTool {
	if mgr == nil {
		panic("builtin: NewStartAsyncTaskTool: manager is required")
	}
	return &StartAsyncTaskTool{manager: mgr}
}

const startAsyncTaskName = "start_async_task"
const startAsyncTaskDescription = "Starts a background worker sub-agent to handle a long-running task. Returns a task_id that can be used with check_async_task."

func (t *StartAsyncTaskTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        startAsyncTaskName,
		Description: startAsyncTaskDescription,
		Parameters: tools.ToolSchema{
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
		},
		Display: tools.DefaultDisplay(startAsyncTaskName, startAsyncTaskDescription),
	}
}

func (t *StartAsyncTaskTool) Execute(ctx context.Context, inputJSON string) (tools.Result, error) {
	sessionKey, ok := agent.SessionKeyFromContext(ctx)
	if !ok {
		return tools.Result{}, fmt.Errorf("tools: sessionKey not found in context")
	}
	var input struct {
		TaskDescription string `json:"task_description"`
		AgentName       string `json:"agent_name"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return tools.Result{}, fmt.Errorf("tools: invalid json: %w", err)
	}
	taskID, err := t.manager.StartTask(ctx, sessionKey, input.AgentName, input.TaskDescription)
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Text(fmt.Sprintf("Background task started. task_id: %s", taskID)), nil
}

// CheckAsyncTaskTool reports the status or final result of an async task.
// Construct via NewCheckAsyncTaskTool.
type CheckAsyncTaskTool struct {
	manager *agent.AsyncTaskManager
}

// NewCheckAsyncTaskTool returns a CheckAsyncTaskTool bound to mgr. Panics if
// mgr is nil.
func NewCheckAsyncTaskTool(mgr *agent.AsyncTaskManager) *CheckAsyncTaskTool {
	if mgr == nil {
		panic("builtin: NewCheckAsyncTaskTool: manager is required")
	}
	return &CheckAsyncTaskTool{manager: mgr}
}

const checkAsyncTaskName = "check_async_task"
const checkAsyncTaskDescription = "Checks the status or retrieves the result of a background async task using its task_id."

func (t *CheckAsyncTaskTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        checkAsyncTaskName,
		Description: checkAsyncTaskDescription,
		Parameters:  asyncTaskIDSchema(),
		Display:     tools.DefaultDisplay(checkAsyncTaskName, checkAsyncTaskDescription),
	}
}

func (t *CheckAsyncTaskTool) Execute(ctx context.Context, inputJSON string) (tools.Result, error) {
	sessionKey, ok := agent.SessionKeyFromContext(ctx)
	if !ok {
		return tools.Result{}, fmt.Errorf("tools: sessionKey not found in context")
	}
	var input struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return tools.Result{}, fmt.Errorf("tools: invalid json: %w", err)
	}

	tasks := t.manager.SyncTasks(ctx, sessionKey)
	task, ok := tasks[input.TaskID]
	if !ok {
		return tools.Result{}, fmt.Errorf("tools: task_id %s not found", input.TaskID)
	}

	if task.Status == "running" {
		return tools.Text(fmt.Sprintf("Task %s is still running. Please check again later.", input.TaskID)), nil
	}
	return tools.Text(fmt.Sprintf("Task %s completed with status: %s\nResult: %s", input.TaskID, task.Status, task.Result)), nil
}

// CancelAsyncTaskTool signals a running async task to stop via its CancelFunc.
// Construct via NewCancelAsyncTaskTool.
type CancelAsyncTaskTool struct {
	manager *agent.AsyncTaskManager
}

// NewCancelAsyncTaskTool returns a CancelAsyncTaskTool bound to mgr. Panics if
// mgr is nil.
func NewCancelAsyncTaskTool(mgr *agent.AsyncTaskManager) *CancelAsyncTaskTool {
	if mgr == nil {
		panic("builtin: NewCancelAsyncTaskTool: manager is required")
	}
	return &CancelAsyncTaskTool{manager: mgr}
}

const cancelAsyncTaskName = "cancel_async_task"
const cancelAsyncTaskDescription = "Cancels a running background task."

func (t *CancelAsyncTaskTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        cancelAsyncTaskName,
		Description: cancelAsyncTaskDescription,
		Parameters:  asyncTaskIDSchema(),
		Display:     tools.DefaultDisplay(cancelAsyncTaskName, cancelAsyncTaskDescription),
	}
}

func (t *CancelAsyncTaskTool) Execute(ctx context.Context, inputJSON string) (tools.Result, error) {
	sessionKey, ok := agent.SessionKeyFromContext(ctx)
	if !ok {
		return tools.Result{}, fmt.Errorf("tools: sessionKey not found in context")
	}
	var input struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return tools.Result{}, fmt.Errorf("tools: invalid json: %w", err)
	}

	if err := t.manager.CancelTask(ctx, sessionKey, input.TaskID); err != nil {
		return tools.Result{}, err
	}
	return tools.Text(fmt.Sprintf("Task %s has been cancelled.", input.TaskID)), nil
}

// UpdateAsyncTaskTool delivers a steering instruction to a running worker.
// Construct via NewUpdateAsyncTaskTool.
type UpdateAsyncTaskTool struct {
	manager *agent.AsyncTaskManager
}

// NewUpdateAsyncTaskTool returns an UpdateAsyncTaskTool bound to mgr. Panics
// if mgr is nil.
func NewUpdateAsyncTaskTool(mgr *agent.AsyncTaskManager) *UpdateAsyncTaskTool {
	if mgr == nil {
		panic("builtin: NewUpdateAsyncTaskTool: manager is required")
	}
	return &UpdateAsyncTaskTool{manager: mgr}
}

const updateAsyncTaskName = "update_async_task"
const updateAsyncTaskDescription = "Sends additional instructions or steers a running background task."

func (t *UpdateAsyncTaskTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        updateAsyncTaskName,
		Description: updateAsyncTaskDescription,
		Parameters: tools.ToolSchema{
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
		},
		Display: tools.DefaultDisplay(updateAsyncTaskName, updateAsyncTaskDescription),
	}
}

func (t *UpdateAsyncTaskTool) Execute(ctx context.Context, inputJSON string) (tools.Result, error) {
	var input struct {
		TaskID      string `json:"task_id"`
		Instruction string `json:"instruction"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return tools.Result{}, fmt.Errorf("tools: invalid json: %w", err)
	}

	if err := t.manager.UpdateTask(input.TaskID, input.Instruction); err != nil {
		return tools.Result{}, err
	}
	return tools.Text(fmt.Sprintf("Instruction sent to task %s.", input.TaskID)), nil
}

// ListAsyncTasksTool enumerates every async task tracked in the current session.
// Construct via NewListAsyncTasksTool.
type ListAsyncTasksTool struct {
	manager *agent.AsyncTaskManager
}

// NewListAsyncTasksTool returns a ListAsyncTasksTool bound to mgr. Panics if
// mgr is nil.
func NewListAsyncTasksTool(mgr *agent.AsyncTaskManager) *ListAsyncTasksTool {
	if mgr == nil {
		panic("builtin: NewListAsyncTasksTool: manager is required")
	}
	return &ListAsyncTasksTool{manager: mgr}
}

const listAsyncTasksName = "list_async_tasks"
const listAsyncTasksDescription = "Lists all async tasks in the current session."

func (t *ListAsyncTasksTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        listAsyncTasksName,
		Description: listAsyncTasksDescription,
		Parameters:  tools.ToolSchema{Type: "object", Properties: map[string]any{}},
		Display:     tools.DefaultDisplay(listAsyncTasksName, listAsyncTasksDescription),
	}
}

func (t *ListAsyncTasksTool) Execute(ctx context.Context, inputJSON string) (tools.Result, error) {
	sessionKey, ok := agent.SessionKeyFromContext(ctx)
	if !ok {
		return tools.Result{}, fmt.Errorf("tools: sessionKey not found in context")
	}
	tasks := t.manager.SyncTasks(ctx, sessionKey)
	if len(tasks) == 0 {
		return tools.Text("No async tasks found in this session."), nil
	}

	var sb strings.Builder
	for _, task := range tasks {
		fmt.Fprintf(&sb, "- %s (Agent: %s, Status: %s)\n", task.TaskID, task.AgentName, task.Status)
	}
	return tools.Text(sb.String()), nil
}
