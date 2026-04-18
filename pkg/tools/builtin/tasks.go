package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// TaskStatus enumerates the task lifecycle states. The LLM is expected to
// move a task from pending → in_progress → completed as it does multi-step
// work; any backward transition is legal (e.g. reopening a completed task).
type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskInProgress TaskStatus = "in_progress"
	TaskCompleted  TaskStatus = "completed"
)

// Task is a single planning entry in the per-session task list. Titles are
// the model's own freeform description; notes are optional detail the model
// may attach on create/update (subtask list, constraints, acceptance
// criteria).
type Task struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Status    TaskStatus `json:"status"`
	Notes     string     `json:"notes,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// TaskStore persists per-session task lists. All methods must be safe for
// concurrent use.
type TaskStore interface {
	Create(ctx context.Context, sessionKey, title, notes string) (Task, error)
	Update(ctx context.Context, sessionKey, id string, status TaskStatus, notes string) (Task, error)
	List(ctx context.Context, sessionKey string) ([]Task, error)
}

// InMemoryTaskStore is a process-local TaskStore. Tasks live as long as the
// process does; they do not survive restart. Sufficient for planning within a
// single run of an agent.
type InMemoryTaskStore struct {
	mu     sync.RWMutex
	data   map[string][]Task // sessionKey → ordered task list
	nextID map[string]int    // sessionKey → next numeric suffix
}

// NewInMemoryTaskStore returns a ready-to-use in-memory task store.
func NewInMemoryTaskStore() *InMemoryTaskStore {
	return &InMemoryTaskStore{
		data:   make(map[string][]Task),
		nextID: make(map[string]int),
	}
}

// Create adds a new pending task to sessionKey and returns it with a freshly
// assigned ID ("t1", "t2", ...).
func (s *InMemoryTaskStore) Create(_ context.Context, sessionKey, title, notes string) (Task, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Task{}, fmt.Errorf("tools: task title is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID[sessionKey]++
	now := time.Now().UTC()
	t := Task{
		ID:        fmt.Sprintf("t%d", s.nextID[sessionKey]),
		Title:     title,
		Status:    TaskPending,
		Notes:     strings.TrimSpace(notes),
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.data[sessionKey] = append(s.data[sessionKey], t)
	return t, nil
}

// Update mutates the task's status (and optionally notes) and returns the
// updated record. Passing an empty notes string leaves the existing notes
// untouched; pass "-" to explicitly clear them.
func (s *InMemoryTaskStore) Update(_ context.Context, sessionKey, id string, status TaskStatus, notes string) (Task, error) {
	if !validStatus(status) {
		return Task{}, fmt.Errorf("tools: invalid status %q (expected pending|in_progress|completed)", status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list, ok := s.data[sessionKey]
	if !ok {
		return Task{}, fmt.Errorf("tools: task %q not found", id)
	}
	for i, t := range list {
		if t.ID != id {
			continue
		}
		t.Status = status
		switch strings.TrimSpace(notes) {
		case "":
			// leave unchanged
		case "-":
			t.Notes = ""
		default:
			t.Notes = strings.TrimSpace(notes)
		}
		t.UpdatedAt = time.Now().UTC()
		list[i] = t
		s.data[sessionKey] = list
		return t, nil
	}
	return Task{}, fmt.Errorf("tools: task %q not found", id)
}

// List returns a snapshot of tasks for sessionKey in creation order.
func (s *InMemoryTaskStore) List(_ context.Context, sessionKey string) ([]Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.data[sessionKey]
	out := make([]Task, len(src))
	copy(out, src)
	return out, nil
}

func validStatus(s TaskStatus) bool {
	switch s {
	case TaskPending, TaskInProgress, TaskCompleted:
		return true
	}
	return false
}

// taskSessionKey mirrors memorySessionKey — tools fail closed when the
// AgentLoop did not inject a session key, so one session cannot read or
// mutate another's task list.
func taskSessionKey(ctx context.Context) (string, error) {
	sk, ok := ctx.Value(agent.SessionKeyCtx("sessionKey")).(string)
	if !ok || sk == "" {
		return "", fmt.Errorf("tools: sessionKey not found in context")
	}
	return sk, nil
}

// ---- CreateTaskTool ----

// CreateTaskTool lets the LLM register a new pending task in the session
// task list as it plans multi-step work.
type CreateTaskTool struct{ Store TaskStore }

func NewCreateTaskTool(store TaskStore) *CreateTaskTool { return &CreateTaskTool{Store: store} }

func (t *CreateTaskTool) Name() string { return "create_task" }
func (t *CreateTaskTool) Description() string {
	return "Register a new pending task in the session task list. Use at the start of multi-step work to plan subtasks; revisit via list_tasks and update via update_task. Title should be short and imperative."
}

type createTaskArgs struct {
	Title string `json:"title" description:"Short imperative description of the task (e.g. 'Write scheduler tests')."`
	Notes string `json:"notes,omitempty" description:"Optional acceptance criteria, constraints, or sub-steps."`
}

func (t *CreateTaskTool) ParametersSchema() tools.ToolSchema { return tools.SchemaFor[createTaskArgs]() }
func (t *CreateTaskTool) RequiresConfirmation() bool         { return false }
func (t *CreateTaskTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	sk, err := taskSessionKey(ctx)
	if err != nil {
		return "", err
	}
	var a createTaskArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}
	if t.Store == nil {
		return "", fmt.Errorf("tools: create_task has no store configured")
	}
	task, err := t.Store.Create(ctx, sk, a.Title, a.Notes)
	if err != nil {
		return "", err
	}
	return marshalOrError(task)
}

// ---- UpdateTaskTool ----

// UpdateTaskTool changes the status (and optionally notes) of an existing
// task. The LLM is expected to flip a task to in_progress when starting it
// and completed when finished, so the list reflects true state.
type UpdateTaskTool struct{ Store TaskStore }

func NewUpdateTaskTool(store TaskStore) *UpdateTaskTool { return &UpdateTaskTool{Store: store} }

func (t *UpdateTaskTool) Name() string { return "update_task" }
func (t *UpdateTaskTool) Description() string {
	return "Update the status (and optional notes) of a task previously registered via create_task. Valid status values: pending, in_progress, completed. Flip to in_progress when you start the task and completed as soon as you finish it."
}

type updateTaskArgs struct {
	ID     string `json:"id" description:"Task ID returned by create_task (e.g. 't1')."`
	Status string `json:"status" description:"New status: 'pending', 'in_progress', or 'completed'." enum:"pending,in_progress,completed"`
	Notes  string `json:"notes,omitempty" description:"Optional new notes. Empty string leaves existing notes unchanged; '-' clears them."`
}

func (t *UpdateTaskTool) ParametersSchema() tools.ToolSchema { return tools.SchemaFor[updateTaskArgs]() }
func (t *UpdateTaskTool) RequiresConfirmation() bool         { return false }
func (t *UpdateTaskTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	sk, err := taskSessionKey(ctx)
	if err != nil {
		return "", err
	}
	var a updateTaskArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("tools: invalid arguments: %w", err)
	}
	if t.Store == nil {
		return "", fmt.Errorf("tools: update_task has no store configured")
	}
	task, err := t.Store.Update(ctx, sk, a.ID, TaskStatus(a.Status), a.Notes)
	if err != nil {
		return "", err
	}
	return marshalOrError(task)
}

// ---- ListTasksTool ----

// ListTasksTool returns all tasks in the current session. The LLM is
// expected to consult this every few turns on long multi-step work to
// re-orient on what's done and what remains.
type ListTasksTool struct{ Store TaskStore }

func NewListTasksTool(store TaskStore) *ListTasksTool { return &ListTasksTool{Store: store} }

func (t *ListTasksTool) Name() string { return "list_tasks" }
func (t *ListTasksTool) Description() string {
	return "Return every task in the session task list with its current status. Call periodically during multi-step work to re-orient on remaining work."
}

type listTasksArgs struct{}

func (t *ListTasksTool) ParametersSchema() tools.ToolSchema { return tools.SchemaFor[listTasksArgs]() }
func (t *ListTasksTool) RequiresConfirmation() bool         { return false }
func (t *ListTasksTool) Execute(ctx context.Context, _ string) (string, error) {
	sk, err := taskSessionKey(ctx)
	if err != nil {
		return "", err
	}
	if t.Store == nil {
		return "", fmt.Errorf("tools: list_tasks has no store configured")
	}
	tasks, err := t.Store.List(ctx, sk)
	if err != nil {
		return "", err
	}
	return marshalOrError(map[string]any{"tasks": tasks, "count": len(tasks)})
}

// RegisterTaskTools wires all three task tools into the given registry
// sharing a single store, so one call enables the entire feature.
//
//	store := builtin.NewInMemoryTaskStore()
//	builtin.RegisterTaskTools(registry, store)
func RegisterTaskTools(registry *tools.Registry, store TaskStore) {
	registry.Register(NewCreateTaskTool(store))
	registry.Register(NewUpdateTaskTool(store))
	registry.Register(NewListTasksTool(store))
}

func marshalOrError(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("tools: marshal task result: %w", err)
	}
	return string(b), nil
}
