package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// ── Test helpers ────────────────────────────────────────────────────────────

// immediateProvider returns Content immediately with no tool calls.
type immediateProvider struct{ content string }

func (p *immediateProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	ch <- Event(ContentEvent{Text: p.content})
	return LLMResult{Content: p.content}, nil
}

// slowProvider blocks until ctx is cancelled or timeout elapses.
type slowProvider struct {
	called int32
}

func (p *slowProvider) GenerateStream(ctx context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	atomic.AddInt32(&p.called, 1)
	select {
	case <-ctx.Done():
		return LLMResult{}, ctx.Err()
	case <-time.After(5 * time.Second):
		ch <- Event(ContentEvent{Text: "slow done"})
		return LLMResult{Content: "slow done"}, nil
	}
}

// historyCapturingProvider records every set of messages it was given so tests
// can assert on what the worker session looked like.
type historyCapturingProvider struct {
	mu      sync.Mutex
	inputs  [][]history.Message
	content string
}

func (p *historyCapturingProvider) GenerateStream(_ context.Context, msgs []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.mu.Lock()
	snapshot := make([]history.Message, len(msgs))
	copy(snapshot, msgs)
	p.inputs = append(p.inputs, snapshot)
	p.mu.Unlock()
	ch <- Event(ContentEvent{Text: p.content})
	return LLMResult{Content: p.content}, nil
}

// ── Tests ───────────────────────────────────────────────────────────────────

func TestAsyncTaskManager_StartTaskReturnsUniqueID(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	mgr := NewAsyncTaskManager(sm, reg, &immediateProvider{content: "ok"})

	seen := map[string]bool{}
	for i := range 3 {
		id, err := mgr.StartTask(context.Background(), "parent", "worker", "do something")
		if err != nil {
			t.Fatalf("StartTask #%d failed: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("duplicate task_id: %s", id)
		}
		seen[id] = true
		// Nanosecond clock collisions are unlikely but possible on very fast
		// hardware — a tiny sleep hardens the test.
		time.Sleep(time.Microsecond)
	}
}

func TestAsyncTaskManager_StartTaskIsNonBlocking(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	mgr := NewAsyncTaskManager(sm, reg, &slowProvider{})

	start := time.Now()
	_, err := mgr.StartTask(context.Background(), "parent", "worker", "slow task")
	if err != nil {
		t.Fatalf("StartTask failed: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("StartTask should return immediately, took %v", time.Since(start))
	}
}

func TestAsyncTaskManager_CancelTaskStopsWorker(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	mgr := NewAsyncTaskManager(sm, reg, &slowProvider{})

	id, err := mgr.StartTask(context.Background(), "parent", "worker", "never ending")
	if err != nil {
		t.Fatalf("StartTask failed: %v", err)
	}
	// Let the worker start its first iteration.
	time.Sleep(50 * time.Millisecond)

	if err := mgr.CancelTask(context.Background(), "parent", id); err != nil {
		t.Fatalf("CancelTask failed: %v", err)
	}
	// Wait for the worker goroutine to exit and persist status.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tasks, _ := sm.AsyncTasks(context.Background(), "parent")
		if task, ok := tasks[id]; ok && task.Status == "cancelled" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task did not reach 'cancelled' status in time")
}

func TestAsyncTaskManager_CancelTaskReturnsErrorForUnknownID(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	mgr := NewAsyncTaskManager(sm, reg, &immediateProvider{content: "ok"})

	err := mgr.CancelTask(context.Background(), "parent", "bogus-id")
	if err == nil {
		t.Fatal("expected error for unknown task_id, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

func TestAsyncTaskManager_UpdateTaskDeliversInstruction(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	prov := &historyCapturingProvider{content: "ack"}
	mgr := NewAsyncTaskManager(sm, reg, prov)

	id, err := mgr.StartTask(context.Background(), "parent", "worker", "first task")
	if err != nil {
		t.Fatalf("StartTask failed: %v", err)
	}
	// Give the worker time to finish its first iteration (immediate provider
	// returns with no tool calls, so the worker exits after one round).
	time.Sleep(200 * time.Millisecond)

	// The worker has already exited — UpdateTask must now report "not active".
	if err := mgr.UpdateTask(id, "extra directive"); err == nil {
		t.Fatal("expected 'not active' error for a completed task")
	}
}

func TestAsyncTaskManager_CleansUpWorkerSession(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	mgr := NewAsyncTaskManager(sm, reg, &immediateProvider{content: "ok"})

	id, err := mgr.StartTask(context.Background(), "parent", "worker", "task body")
	if err != nil {
		t.Fatalf("StartTask failed: %v", err)
	}

	// Wait for the worker to finish and clean up.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tasks, _ := sm.AsyncTasks(context.Background(), "parent")
		if task, ok := tasks[id]; ok && task.Status == "success" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// GetHistory returns a fresh system-prompt-only slice when a session is
	// unknown, so length==1 with role "system" indicates the worker session
	// was deleted (not persisted state).
	hist, _ := sm.History(context.Background(), id)
	if len(hist) != 1 || hist[0].Role != "system" {
		t.Fatalf("expected worker session to be deleted, got %d msgs: %+v", len(hist), hist)
	}
}

func TestAsyncTaskManager_MaxConcurrentRejectsOverflow(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	// slowProvider keeps each worker blocked until ctx cancellation, so we can
	// saturate the semaphore deterministically.
	mgr := NewAsyncTaskManager(sm, reg, &slowProvider{}).WithMaxConcurrent(2)

	id1, err := mgr.StartTask(context.Background(), "parent", "w", "task-1")
	if err != nil {
		t.Fatalf("task-1 should start, got %v", err)
	}
	id2, err := mgr.StartTask(context.Background(), "parent", "w", "task-2")
	if err != nil {
		t.Fatalf("task-2 should start, got %v", err)
	}

	// 3rd task exceeds the cap — must be rejected, not queued.
	if _, err := mgr.StartTask(context.Background(), "parent", "w", "task-3"); err == nil {
		t.Fatal("expected StartTask to reject when cap is reached")
	} else if !strings.Contains(err.Error(), "cap reached") {
		t.Fatalf("expected 'cap reached' error, got: %v", err)
	}

	// Cancel one — a slot must free up and the next StartTask should succeed.
	if err := mgr.CancelTask(context.Background(), "parent", id1); err != nil {
		t.Fatalf("cancel task-1: %v", err)
	}
	// Wait for the cancelled goroutine to release the semaphore slot.
	deadline := time.Now().Add(2 * time.Second)
	var id3 string
	for time.Now().Before(deadline) {
		id3, err = mgr.StartTask(context.Background(), "parent", "w", "task-3b")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("expected slot to free after cancel, still got %v", err)
	}

	// Clean up remaining tasks so the test exits promptly.
	_ = mgr.CancelTask(context.Background(), "parent", id2)
	_ = mgr.CancelTask(context.Background(), "parent", id3)
}

func TestAsyncTaskManager_SyncTasksMarksOrphanedAsInterrupted(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	mgr := NewAsyncTaskManager(sm, reg, &immediateProvider{content: "ok"})

	// Simulate a persisted "running" task that isn't in ActiveTasks
	// (e.g., the process restarted mid-task).
	tasks := map[string]history.AsyncTask{
		"orphan-1": {TaskID: "orphan-1", AgentName: "w", Status: "running"},
	}
	sm.SaveAsyncTasks(context.Background(), "parent", tasks)

	got := mgr.SyncTasks(context.Background(), "parent")
	if got["orphan-1"].Status != "interrupted" {
		t.Fatalf("expected orphan marked as 'interrupted', got %q", got["orphan-1"].Status)
	}
}
