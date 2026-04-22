// Package historyfake provides a test-friendly SessionManager fake for
// consumers writing agent-integration tests without standing up a real
// session backend. The fake stores everything in-memory, exposes hooks for
// asserting call counts and replacing return values, and is safe for
// concurrent use.
//
// Typical use:
//
//	sm := historyfake.NewSessionManager().
//	    Seed("s1", []history.Message{{Role: "system", Content: "be helpful"}})
//	loop := agent.NewAgentLoop(sm, reg, provider)
//	// ... run scenarios ...
//	if got := sm.Calls().Get; got != 2 {
//	    t.Fatalf("expected 2 GetHistory calls, got %d", got)
//	}
package historyfake

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// SessionManager is an in-memory agent.SessionManager suitable for tests.
// Every method is safe for concurrent calls.
type SessionManager struct {
	mu        sync.RWMutex
	messages  map[string][]history.Message
	asyncMap  map[string]map[string]history.AsyncTask

	// SaveErr, if set, is returned from every Save call.
	SaveErr error
	// DeleteErr, if set, is returned from every DeleteSession call.
	DeleteErr error
	// ForkErr, if set, is returned from every Fork call.
	ForkErr error

	calls CallStats
}

// CallStats records how many times each method fired. Read via Calls().
type CallStats struct {
	Get       atomic.Int64
	Set       atomic.Int64
	GetAsync  atomic.Int64
	SetAsync  atomic.Int64
	Save      atomic.Int64
	Delete    atomic.Int64
	Fork      atomic.Int64
}

// NewSessionManager returns an empty fake. Use Seed to pre-populate history.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		messages: make(map[string][]history.Message),
		asyncMap: make(map[string]map[string]history.AsyncTask),
	}
}

// Seed installs a starting message slice for sessionKey. Returns the manager
// so it chains in a test setup line.
func (sm *SessionManager) Seed(sessionKey string, msgs []history.Message) *SessionManager {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cp := make([]history.Message, len(msgs))
	copy(cp, msgs)
	sm.messages[sessionKey] = cp
	return sm
}

// Calls returns a snapshot of the call counters. Returned values are stable
// — they reflect the state at the moment of the call.
func (sm *SessionManager) Calls() CallStatsSnapshot {
	return CallStatsSnapshot{
		Get:      sm.calls.Get.Load(),
		Set:      sm.calls.Set.Load(),
		GetAsync: sm.calls.GetAsync.Load(),
		SetAsync: sm.calls.SetAsync.Load(),
		Save:     sm.calls.Save.Load(),
		Delete:   sm.calls.Delete.Load(),
		Fork:     sm.calls.Fork.Load(),
	}
}

// CallStatsSnapshot is a point-in-time view of CallStats, returned by Calls().
type CallStatsSnapshot struct {
	Get      int64
	Set      int64
	GetAsync int64
	SetAsync int64
	Save     int64
	Delete   int64
	Fork     int64
}

// GetHistory implements agent.SessionManager.
func (sm *SessionManager) GetHistory(_ context.Context, sessionKey string) []history.Message {
	sm.calls.Get.Add(1)
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	src, ok := sm.messages[sessionKey]
	if !ok {
		return nil
	}
	cp := make([]history.Message, len(src))
	copy(cp, src)
	return cp
}

// SetHistory implements agent.SessionManager.
func (sm *SessionManager) SetHistory(_ context.Context, sessionKey string, messages []history.Message) {
	sm.calls.Set.Add(1)
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cp := make([]history.Message, len(messages))
	copy(cp, messages)
	sm.messages[sessionKey] = cp
}

// GetAsyncTasks implements agent.SessionManager.
func (sm *SessionManager) GetAsyncTasks(_ context.Context, sessionKey string) map[string]history.AsyncTask {
	sm.calls.GetAsync.Add(1)
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	src, ok := sm.asyncMap[sessionKey]
	if !ok {
		return map[string]history.AsyncTask{}
	}
	cp := make(map[string]history.AsyncTask, len(src))
	for k, v := range src {
		cp[k] = v
	}
	return cp
}

// SetAsyncTasks implements agent.SessionManager.
func (sm *SessionManager) SetAsyncTasks(_ context.Context, sessionKey string, tasks map[string]history.AsyncTask) {
	sm.calls.SetAsync.Add(1)
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cp := make(map[string]history.AsyncTask, len(tasks))
	for k, v := range tasks {
		cp[k] = v
	}
	sm.asyncMap[sessionKey] = cp
}

// Save implements agent.SessionManager. Returns SaveErr if configured.
func (sm *SessionManager) Save(_ context.Context, _ string) error {
	sm.calls.Save.Add(1)
	return sm.SaveErr
}

// DeleteSession implements agent.SessionManager. Returns DeleteErr if
// configured; otherwise removes the session's state.
func (sm *SessionManager) DeleteSession(_ context.Context, sessionKey string) error {
	sm.calls.Delete.Add(1)
	if sm.DeleteErr != nil {
		return sm.DeleteErr
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.messages, sessionKey)
	delete(sm.asyncMap, sessionKey)
	return nil
}

// Fork implements agent.SessionManager. Returns ForkErr if configured;
// otherwise copies the first atIndex messages into a new key "<src>-fork-N"
// and returns it.
func (sm *SessionManager) Fork(_ context.Context, sessionKey string, atIndex int) (string, error) {
	sm.calls.Fork.Add(1)
	if sm.ForkErr != nil {
		return "", sm.ForkErr
	}
	if atIndex < 0 {
		return "", fmt.Errorf("historyfake: fork atIndex must be >= 0, got %d", atIndex)
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	src, ok := sm.messages[sessionKey]
	if !ok {
		return "", fmt.Errorf("historyfake: session %q not found", sessionKey)
	}
	end := atIndex
	if end > len(src) {
		end = len(src)
	}
	newKey := fmt.Sprintf("%s-fork-%d", sessionKey, sm.calls.Fork.Load())
	cp := make([]history.Message, end)
	copy(cp, src[:end])
	sm.messages[newKey] = cp
	return newKey, nil
}
