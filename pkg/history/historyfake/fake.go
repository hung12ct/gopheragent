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
//	if got := sm.Calls().History; got != 2 {
//	    t.Fatalf("expected 2 History calls, got %d", got)
//	}
package historyfake

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// SessionManager is an in-memory agent.SessionManager suitable for tests.
// Every method is safe for concurrent calls.
type SessionManager struct {
	mu       sync.RWMutex
	messages map[string][]history.Message
	asyncMap map[string]map[string]history.AsyncTask
	titles   map[string]string

	// SaveErr, if set, is returned from every SaveHistory / SaveAsyncTasks call.
	SaveErr error
	// DeleteErr, if set, is returned from every Delete call.
	DeleteErr error
	// ForkErr, if set, is returned from every Fork call.
	ForkErr error

	calls CallStats
}

// CallStats records how many times each method fired. Read via Calls().
type CallStats struct {
	History        atomic.Int64
	SaveHistory    atomic.Int64
	AsyncTasks     atomic.Int64
	SaveAsyncTasks atomic.Int64
	Delete         atomic.Int64
	Fork           atomic.Int64
}

// NewSessionManager returns an empty fake. Use Seed to pre-populate history.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		messages: make(map[string][]history.Message),
		asyncMap: make(map[string]map[string]history.AsyncTask),
		titles:   make(map[string]string),
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
		History:        sm.calls.History.Load(),
		SaveHistory:    sm.calls.SaveHistory.Load(),
		AsyncTasks:     sm.calls.AsyncTasks.Load(),
		SaveAsyncTasks: sm.calls.SaveAsyncTasks.Load(),
		Delete:         sm.calls.Delete.Load(),
		Fork:           sm.calls.Fork.Load(),
	}
}

// CallStatsSnapshot is a point-in-time view of CallStats, returned by Calls().
type CallStatsSnapshot struct {
	History        int64
	SaveHistory    int64
	AsyncTasks     int64
	SaveAsyncTasks int64
	Delete         int64
	Fork           int64
}

// History implements agent.SessionManager.
func (sm *SessionManager) History(_ context.Context, sessionKey string) ([]history.Message, error) {
	sm.calls.History.Add(1)
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	src, ok := sm.messages[sessionKey]
	if !ok {
		return nil, nil
	}
	cp := make([]history.Message, len(src))
	copy(cp, src)
	return cp, nil
}

// SaveHistory implements agent.SessionManager. Returns SaveErr if configured.
func (sm *SessionManager) SaveHistory(_ context.Context, sessionKey string, msgs []history.Message) error {
	sm.calls.SaveHistory.Add(1)
	if sm.SaveErr != nil {
		return sm.SaveErr
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cp := make([]history.Message, len(msgs))
	copy(cp, msgs)
	sm.messages[sessionKey] = cp
	return nil
}

// AsyncTasks implements agent.SessionManager.
func (sm *SessionManager) AsyncTasks(_ context.Context, sessionKey string) (map[string]history.AsyncTask, error) {
	sm.calls.AsyncTasks.Add(1)
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	src, ok := sm.asyncMap[sessionKey]
	if !ok {
		return map[string]history.AsyncTask{}, nil
	}
	cp := make(map[string]history.AsyncTask, len(src))
	for k, v := range src {
		cp[k] = v
	}
	return cp, nil
}

// SaveAsyncTasks implements agent.SessionManager. Returns SaveErr if configured.
func (sm *SessionManager) SaveAsyncTasks(_ context.Context, sessionKey string, tasks map[string]history.AsyncTask) error {
	sm.calls.SaveAsyncTasks.Add(1)
	if sm.SaveErr != nil {
		return sm.SaveErr
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cp := make(map[string]history.AsyncTask, len(tasks))
	for k, v := range tasks {
		cp[k] = v
	}
	sm.asyncMap[sessionKey] = cp
	return nil
}

// Delete implements agent.SessionManager. Returns DeleteErr if configured;
// otherwise removes the session's state.
func (sm *SessionManager) Delete(_ context.Context, sessionKey string) error {
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

// Query implements agent.SessionManager. Returns metadata for every fake
// session whose key starts with prefix; intended for unit tests, so the
// implementation is intentionally simple — no soft-delete tracking, no
// ordering beyond opts.OrderBy.
func (sm *SessionManager) Query(_ context.Context, prefix string, opts history.SessionQueryOpts) ([]history.SessionMeta, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make([]history.SessionMeta, 0, len(sm.messages))
	for key, msgs := range sm.messages {
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		out = append(out, history.SessionMeta{Key: key, MessageCount: len(msgs), Title: sm.titles[key]})
	}
	if opts.Limit > 0 && opts.Limit < len(out) {
		out = out[:opts.Limit]
	}
	return out, nil
}

// SetTitle implements agent.SessionTitler. Stores the title in-memory so
// the fake's Query results carry it. Empty title clears the entry.
func (sm *SessionManager) SetTitle(_ context.Context, sessionKey string, title string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if title == "" {
		delete(sm.titles, sessionKey)
	} else {
		sm.titles[sessionKey] = title
	}
	return nil
}

// SoftDelete implements agent.SessionManager. The fake forwards to Delete —
// tests that need true soft-delete semantics should implement their own
// SessionManager.
func (sm *SessionManager) SoftDelete(ctx context.Context, sessionKey string) error {
	return sm.Delete(ctx, sessionKey)
}

// Restore implements agent.SessionManager. The fake has no tombstone state,
// so this is always a no-op.
func (sm *SessionManager) Restore(_ context.Context, _ string) error { return nil }

// PurgeDeletedBefore implements agent.SessionManager. The fake has no
// tombstone state, so this is always a no-op.
func (sm *SessionManager) PurgeDeletedBefore(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
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
	end := min(atIndex, len(src))
	newKey := fmt.Sprintf("%s-fork-%d", sessionKey, sm.calls.Fork.Load())
	cp := make([]history.Message, end)
	copy(cp, src[:end])
	sm.messages[newKey] = cp
	return newKey, nil
}
