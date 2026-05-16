package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func taskCtx(sessionKey string) context.Context {
	return agent.WithSessionKey(context.Background(), sessionKey)
}

func TestInMemoryTaskStore_CreateAssignsSequentialIDs(t *testing.T) {
	s := NewInMemoryTaskStore()
	ctx := context.Background()
	t1, err := s.Create(ctx, "s1", "first", "")
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	t2, err := s.Create(ctx, "s1", "second", "notes")
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if t1.ID != "t1" || t2.ID != "t2" {
		t.Fatalf("IDs = %q,%q; want t1,t2", t1.ID, t2.ID)
	}
	if t1.Status != TaskPending || t2.Status != TaskPending {
		t.Fatalf("new tasks should be pending, got %q,%q", t1.Status, t2.Status)
	}
	if t2.Notes != "notes" {
		t.Fatalf("notes = %q; want %q", t2.Notes, "notes")
	}
	if t1.CreatedAt.IsZero() || t1.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not set: %+v", t1)
	}
}

func TestInMemoryTaskStore_CreateRejectsEmptyTitle(t *testing.T) {
	s := NewInMemoryTaskStore()
	if _, err := s.Create(context.Background(), "s1", "   ", ""); err == nil {
		t.Fatal("expected error for empty title")
	}
}

func TestInMemoryTaskStore_UpdateStatusAndNotes(t *testing.T) {
	s := NewInMemoryTaskStore()
	ctx := context.Background()
	created, _ := s.Create(ctx, "s1", "task", "initial notes")

	// Update status; empty notes should leave existing notes unchanged.
	u1, err := s.Update(ctx, "s1", created.ID, TaskInProgress, "")
	if err != nil {
		t.Fatalf("update 1: %v", err)
	}
	if u1.Status != TaskInProgress {
		t.Fatalf("status = %q; want in_progress", u1.Status)
	}
	if u1.Notes != "initial notes" {
		t.Fatalf("notes should be unchanged, got %q", u1.Notes)
	}
	if !u1.UpdatedAt.After(created.UpdatedAt) && !u1.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("UpdatedAt regressed: %v -> %v", created.UpdatedAt, u1.UpdatedAt)
	}

	// Replacement.
	u2, _ := s.Update(ctx, "s1", created.ID, TaskCompleted, "new notes")
	if u2.Notes != "new notes" || u2.Status != TaskCompleted {
		t.Fatalf("unexpected state: %+v", u2)
	}

	// Clear via "-".
	u3, _ := s.Update(ctx, "s1", created.ID, TaskCompleted, "-")
	if u3.Notes != "" {
		t.Fatalf("notes should be cleared, got %q", u3.Notes)
	}
}

func TestInMemoryTaskStore_UpdateInvalidStatus(t *testing.T) {
	s := NewInMemoryTaskStore()
	created, _ := s.Create(context.Background(), "s1", "task", "")
	_, err := s.Update(context.Background(), "s1", created.ID, TaskStatus("bogus"), "")
	if err == nil || !strings.Contains(err.Error(), "invalid status") {
		t.Fatalf("expected invalid-status error, got %v", err)
	}
}

func TestInMemoryTaskStore_UpdateUnknownID(t *testing.T) {
	s := NewInMemoryTaskStore()
	_, err := s.Update(context.Background(), "s1", "t99", TaskCompleted, "")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found, got %v", err)
	}
	_, _ = s.Create(context.Background(), "s1", "t", "")
	_, err = s.Update(context.Background(), "s1", "t99", TaskCompleted, "")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found, got %v", err)
	}
}

func TestInMemoryTaskStore_ListOrderedAndSnapshot(t *testing.T) {
	s := NewInMemoryTaskStore()
	ctx := context.Background()
	for _, title := range []string{"one", "two", "three"} {
		if _, err := s.Create(ctx, "s1", title, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ctx, "s1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d; want 3", len(got))
	}
	if got[0].Title != "one" || got[1].Title != "two" || got[2].Title != "three" {
		t.Fatalf("unexpected order: %+v", got)
	}
	// Mutating the snapshot must not affect the store.
	got[0].Title = "mutated"
	again, _ := s.List(ctx, "s1")
	if again[0].Title != "one" {
		t.Fatal("list did not return a snapshot; caller can mutate internal state")
	}
}

func TestInMemoryTaskStore_SessionsAreIsolated(t *testing.T) {
	s := NewInMemoryTaskStore()
	ctx := context.Background()
	a1, _ := s.Create(ctx, "a", "only-a", "")
	b1, _ := s.Create(ctx, "b", "only-b", "")
	if a1.ID != "t1" || b1.ID != "t1" {
		t.Fatalf("each session should start ID counter at t1; got a=%q b=%q", a1.ID, b1.ID)
	}
	listA, _ := s.List(ctx, "a")
	listB, _ := s.List(ctx, "b")
	if len(listA) != 1 || listA[0].Title != "only-a" {
		t.Fatalf("session a list: %+v", listA)
	}
	if len(listB) != 1 || listB[0].Title != "only-b" {
		t.Fatalf("session b list: %+v", listB)
	}
}

func TestCreateTaskTool_HappyPath(t *testing.T) {
	store := NewInMemoryTaskStore()
	tool := NewCreateTaskTool(store)
	out, err := tool.Execute(taskCtx("s1"), `{"title":"plan","notes":"details"}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var got Task
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, out)
	}
	if got.ID != "t1" || got.Title != "plan" || got.Status != TaskPending || got.Notes != "details" {
		t.Fatalf("unexpected task: %+v", got)
	}
}

func TestCreateTaskTool_RejectsMissingSessionKey(t *testing.T) {
	tool := NewCreateTaskTool(NewInMemoryTaskStore())
	_, err := tool.Execute(context.Background(), `{"title":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "sessionKey") {
		t.Fatalf("expected sessionKey error, got %v", err)
	}
}

func TestCreateTaskTool_InvalidJSON(t *testing.T) {
	tool := NewCreateTaskTool(NewInMemoryTaskStore())
	_, err := tool.Execute(taskCtx("s1"), `{not json`)
	if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("expected invalid-arguments error, got %v", err)
	}
}

func TestUpdateTaskTool_HappyPath(t *testing.T) {
	store := NewInMemoryTaskStore()
	create := NewCreateTaskTool(store)
	update := NewUpdateTaskTool(store)
	ctx := taskCtx("s1")

	if _, err := create.Execute(ctx, `{"title":"task"}`); err != nil {
		t.Fatal(err)
	}
	out, err := update.Execute(ctx, `{"id":"t1","status":"in_progress","notes":"working"}`)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var got Task
	_ = json.Unmarshal([]byte(out), &got)
	if got.Status != TaskInProgress || got.Notes != "working" {
		t.Fatalf("unexpected state: %+v", got)
	}
}

func TestUpdateTaskTool_InvalidStatusBubblesUp(t *testing.T) {
	store := NewInMemoryTaskStore()
	_, _ = store.Create(context.Background(), "s1", "t", "")
	tool := NewUpdateTaskTool(store)
	_, err := tool.Execute(taskCtx("s1"), `{"id":"t1","status":"bogus"}`)
	if err == nil || !strings.Contains(err.Error(), "invalid status") {
		t.Fatalf("expected invalid-status error, got %v", err)
	}
}

func TestListTasksTool_ReturnsEnvelope(t *testing.T) {
	store := NewInMemoryTaskStore()
	ctx := taskCtx("s1")
	_, _ = store.Create(context.Background(), "s1", "first", "")
	_, _ = store.Create(context.Background(), "s1", "second", "")

	list := NewListTasksTool(store)
	out, err := list.Execute(ctx, ``)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var env struct {
		Tasks []Task `json:"tasks"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, out)
	}
	if env.Count != 2 || len(env.Tasks) != 2 {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	if env.Tasks[0].Title != "first" || env.Tasks[1].Title != "second" {
		t.Fatalf("unexpected order: %+v", env.Tasks)
	}
}

func TestListTasksTool_EmptySession(t *testing.T) {
	list := NewListTasksTool(NewInMemoryTaskStore())
	out, err := list.Execute(taskCtx("s1"), ``)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, `"count":0`) {
		t.Fatalf("expected empty count, got %s", out)
	}
}

func TestRegisterTaskTools_RegistersAllThree(t *testing.T) {
	registry := tools.NewRegistry()
	store := NewInMemoryTaskStore()
	RegisterTaskTools(registry, store)
	for _, name := range []string{"create_task", "update_task", "list_tasks"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("tool %q not registered", name)
		}
	}
}

func TestTaskTools_EmitsTaskListOnMutation(t *testing.T) {
	store := NewInMemoryTaskStore()
	create := NewCreateTaskTool(store)
	update := NewUpdateTaskTool(store)

	var emitted []agent.StreamEvent
	ctx := agent.WithSubAgentEmitter(taskCtx("s1"), func(ev agent.StreamEvent) {
		emitted = append(emitted, ev)
	})

	if _, err := create.Execute(ctx, `{"title":"plan"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := update.Execute(ctx, `{"id":"t1","status":"completed"}`); err != nil {
		t.Fatal(err)
	}

	// One event per successful mutation, both EventTypeTaskList.
	if len(emitted) != 2 {
		t.Fatalf("expected 2 task_list events, got %d", len(emitted))
	}
	for i, ev := range emitted {
		if ev.Type != agent.EventTypeTaskList {
			t.Fatalf("event %d type = %q; want task_list", i, ev.Type)
		}
	}

	// The second event must reflect the completed status — this is the
	// state the frontend will render with strikethrough.
	var items []agent.TaskListItem
	if err := json.Unmarshal([]byte(emitted[1].Content), &items); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(items) != 1 || items[0].Status != string(TaskCompleted) {
		t.Fatalf("expected single completed task, got %+v", items)
	}
}

func TestTaskTools_NoEmitterIsSafe(t *testing.T) {
	// No emitter installed on ctx — tools must succeed without panicking.
	store := NewInMemoryTaskStore()
	create := NewCreateTaskTool(store)
	if _, err := create.Execute(taskCtx("s1"), `{"title":"x"}`); err != nil {
		t.Fatalf("create without emitter: %v", err)
	}
}

func TestTaskTools_SessionsAreIsolatedViaContext(t *testing.T) {
	store := NewInMemoryTaskStore()
	create := NewCreateTaskTool(store)
	list := NewListTasksTool(store)

	if _, err := create.Execute(taskCtx("alpha"), `{"title":"only-alpha"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := create.Execute(taskCtx("beta"), `{"title":"only-beta"}`); err != nil {
		t.Fatal(err)
	}

	outA, _ := list.Execute(taskCtx("alpha"), ``)
	outB, _ := list.Execute(taskCtx("beta"), ``)
	if !strings.Contains(outA, "only-alpha") || strings.Contains(outA, "only-beta") {
		t.Fatalf("alpha leaked: %s", outA)
	}
	if !strings.Contains(outB, "only-beta") || strings.Contains(outB, "only-alpha") {
		t.Fatalf("beta leaked: %s", outB)
	}
}
