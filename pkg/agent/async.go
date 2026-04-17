package agent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// ActiveTask is the in-memory handle for a running async task. It is removed
// from AsyncTaskManager.ActiveTasks once the underlying goroutine exits.
type ActiveTask struct {
	// CancelFunc stops the worker goroutine.
	CancelFunc context.CancelFunc
	// InstructionChan delivers steering instructions from the main agent to
	// the worker. Buffered — senders never block, but excess instructions are
	// drained at each iteration boundary.
	InstructionChan chan string
}

// AsyncTaskManager owns the lifecycle of background worker agents. Each task
// runs a separate AgentLoop in its own goroutine with an isolated session key.
// The task metadata (status, result) is persisted under the *parent* session's
// AsyncTasks map, while the worker's message history lives under a dedicated
// per-task session key that is deleted when the worker exits.
//
// When MaxConcurrent is greater than zero, StartTask rejects new tasks beyond
// that cap with an error rather than queueing. This is a backpressure signal —
// the caller decides whether to retry, drop, or surface to the user.
type AsyncTaskManager struct {
	mu          sync.RWMutex
	ActiveTasks map[string]*ActiveTask
	Sessions    SessionManager
	Tools       *tools.Registry
	LLM         LLMProvider

	// MaxConcurrent caps the number of simultaneously running worker
	// goroutines. 0 means unlimited (the default). Set via WithMaxConcurrent.
	MaxConcurrent int
	sem           chan struct{}
}

// NewAsyncTaskManager constructs a manager wired to the given dependencies.
func NewAsyncTaskManager(sessions SessionManager, registry *tools.Registry, llm LLMProvider) *AsyncTaskManager {
	return &AsyncTaskManager{
		ActiveTasks: make(map[string]*ActiveTask),
		Sessions:    sessions,
		Tools:       registry,
		LLM:         llm,
	}
}

// WithMaxConcurrent caps the number of simultaneously running worker
// goroutines at n. StartTask returns an error when the cap is reached.
// n == 0 removes the cap (unlimited). Typically called once right after
// NewAsyncTaskManager; changing the cap while tasks are active is allowed
// but the new cap only applies to subsequent StartTask calls.
func (m *AsyncTaskManager) WithMaxConcurrent(n int) *AsyncTaskManager {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MaxConcurrent = n
	if n > 0 {
		m.sem = make(chan struct{}, n)
	} else {
		m.sem = nil
	}
	return m
}

// StartTask registers a new async task and launches the worker goroutine.
// Returns the task_id immediately; the worker runs detached from parentCtx so
// it survives the HTTP request that spawned it.
//
// Session writes here use context.Background(): the task metadata must outlive
// the caller's request even if the caller is cancelled immediately after this
// returns.
func (m *AsyncTaskManager) StartTask(_ context.Context, sessionKey, agentName, description string) (string, error) {
	// Enforce the concurrency cap. Non-blocking acquire — a full semaphore
	// means the caller asked for more than the cap allows.
	m.mu.RLock()
	sem := m.sem
	cap := m.MaxConcurrent
	m.mu.RUnlock()
	if sem != nil {
		select {
		case sem <- struct{}{}:
		default:
			return "", fmt.Errorf("agent: async task cap reached (%d in flight)", cap)
		}
	}

	taskID := fmt.Sprintf("async-%s-%d", agentName, time.Now().UnixNano())

	workerCtx, cancel := context.WithCancel(context.Background())

	m.mu.Lock()
	m.ActiveTasks[taskID] = &ActiveTask{
		CancelFunc:      cancel,
		InstructionChan: make(chan string, 10),
	}
	m.mu.Unlock()

	ctx := context.Background()
	tasks := m.Sessions.GetAsyncTasks(ctx, sessionKey)
	tasks[taskID] = history.AsyncTask{
		TaskID:    taskID,
		AgentName: agentName,
		Status:    "running",
	}
	m.Sessions.SetAsyncTasks(ctx, sessionKey, tasks)
	if err := m.Sessions.Save(ctx, sessionKey); err != nil {
		log.Printf("[async] save failed for %q: %v", sessionKey, err)
	}

	go m.runLoop(workerCtx, taskID, sessionKey, agentName, description)

	return taskID, nil
}

// runLoop is the worker goroutine body. It creates a fresh AgentLoop with an
// isolated session and runs until completion, cancellation, or error. The
// worker session is deleted on exit so history does not accumulate.
func (m *AsyncTaskManager) runLoop(ctx context.Context, taskID, sessionKey, agentName, initialDesc string) {
	workerSessionKey := taskID
	defer func() {
		// Release the concurrency slot first so a waiting caller can proceed
		// even if session cleanup is slow.
		m.mu.RLock()
		sem := m.sem
		m.mu.RUnlock()
		if sem != nil {
			<-sem
		}
		if err := m.Sessions.DeleteSession(context.Background(), workerSessionKey); err != nil {
			log.Printf("[async] worker session cleanup failed for %q: %v", workerSessionKey, err)
		}
	}()

	workerLoop := NewAgentLoop(m.Sessions, m.Tools, m.LLM)
	workerLoop.MaxIters = 20
	workerLoop.EmitThoughts = false

	workerLoop.BeforeHooks = append(workerLoop.BeforeHooks, func(hookCtx context.Context, _ string, _ string) error {
		m.mu.RLock()
		task, ok := m.ActiveTasks[taskID]
		m.mu.RUnlock()
		if !ok {
			return nil
		}
		// Drain all pending instructions so none are lost when multiple
		// updates arrive during a single iteration.
		var collected []string
	drain:
		for {
			select {
			case inst := <-task.InstructionChan:
				collected = append(collected, inst)
			default:
				break drain
			}
		}
		if len(collected) > 0 {
			msgs := m.Sessions.GetHistory(hookCtx, workerSessionKey)
			for _, inst := range collected {
				msgs = append(msgs, history.Message{Role: "user", Content: "[Update from Main Agent]: " + inst})
			}
			m.Sessions.SetHistory(hookCtx, workerSessionKey, msgs)
		}
		return nil
	})

	instruction := fmt.Sprintf("[%s Async Worker]\nTask: %s", agentName, initialDesc)
	result, err := workerLoop.RunIteration(ctx, workerSessionKey, instruction)

	finalStatus := "success"
	finalResult := result
	if err != nil {
		if ctx.Err() != nil {
			finalStatus = "cancelled"
			finalResult = "Task cancelled by parent."
		} else {
			finalStatus = "error"
			finalResult = err.Error()
		}
	}

	bgCtx := context.Background()
	tasks := m.Sessions.GetAsyncTasks(bgCtx, sessionKey)
	if task, exists := tasks[taskID]; exists {
		task.Status = finalStatus
		task.Result = finalResult
		tasks[taskID] = task
		m.Sessions.SetAsyncTasks(bgCtx, sessionKey, tasks)
		if err := m.Sessions.Save(bgCtx, sessionKey); err != nil {
			log.Printf("[async] save failed for %q: %v", sessionKey, err)
		}
	}

	m.mu.Lock()
	delete(m.ActiveTasks, taskID)
	m.mu.Unlock()
}

// CancelTask signals the worker goroutine to stop and marks the task as
// cancelled in the parent session. Returns an error when taskID is not
// currently active.
func (m *AsyncTaskManager) CancelTask(sessionKey, taskID string) error {
	m.mu.Lock()
	task, ok := m.ActiveTasks[taskID]
	if ok {
		task.CancelFunc()
		delete(m.ActiveTasks, taskID)
	}
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("agent: task %q not found or already completed", taskID)
	}

	bgCtx := context.Background()
	tasks := m.Sessions.GetAsyncTasks(bgCtx, sessionKey)
	if task, exists := tasks[taskID]; exists {
		task.Status = "cancelled"
		tasks[taskID] = task
		m.Sessions.SetAsyncTasks(bgCtx, sessionKey, tasks)
		if err := m.Sessions.Save(bgCtx, sessionKey); err != nil {
			log.Printf("[async] save failed for %q: %v", sessionKey, err)
		}
	}
	return nil
}

// UpdateTask delivers a steering instruction to a running worker. It is
// non-blocking: if the worker's buffer is full, the instruction is dropped.
func (m *AsyncTaskManager) UpdateTask(taskID, instruction string) error {
	m.mu.RLock()
	task, ok := m.ActiveTasks[taskID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("agent: task %q is not active or not running locally", taskID)
	}
	select {
	case task.InstructionChan <- instruction:
	default:
	}
	return nil
}

// SyncTasks reconciles persisted task metadata with the manager's in-memory
// ActiveTasks map. Tasks whose status says "running" but are not present in
// ActiveTasks are marked "interrupted" (e.g., the process restarted).
// Returns a snapshot copy of the reconciled task map.
func (m *AsyncTaskManager) SyncTasks(sessionKey string) map[string]history.AsyncTask {
	bgCtx := context.Background()
	tasks := m.Sessions.GetAsyncTasks(bgCtx, sessionKey)
	changed := false
	m.mu.RLock()
	for id, t := range tasks {
		if t.Status == "running" {
			if _, ok := m.ActiveTasks[id]; !ok {
				t.Status = "interrupted"
				t.Result = "Process restarted before task could finish."
				tasks[id] = t
				changed = true
			}
		}
	}
	m.mu.RUnlock()
	if changed {
		m.Sessions.SetAsyncTasks(bgCtx, sessionKey, tasks)
		if err := m.Sessions.Save(bgCtx, sessionKey); err != nil {
			log.Printf("[async] save failed for %q: %v", sessionKey, err)
		}
	}
	return tasks
}
