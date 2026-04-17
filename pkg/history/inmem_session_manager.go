package history

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const backgroundSumTriggerThreshold = 20
const backgroundSumNewMsgThreshold = 10 // min new messages since last summarization before re-triggering

// InMemSessionManager is a thread-safe in-memory session store with optional TTL eviction.
type InMemSessionManager struct {
	mu              sync.RWMutex
	sessions        map[string][]Message
	asyncTasks      map[string]map[string]AsyncTask
	behaviors       map[string]string
	lastSumLen      map[string]int
	SystemPrompt    string
	SummaryProvider SummaryProvider // if nil, background summarization is disabled

	// TTL is the idle expiry duration per session (0 = never expire).
	// A session is evicted when it has not been read or written for TTL duration.
	// Call StartCleanup to activate background eviction.
	TTL     time.Duration
	lastUse sync.Map // sessionKey → time.Time
}

// NewInMemSessionManager creates a new in-memory session manager.
// An optional systemPrompt can be provided; defaults to a generic assistant prompt.
func NewInMemSessionManager(systemPrompt ...string) *InMemSessionManager {
	sp := "You are an AI assistant."
	if len(systemPrompt) > 0 && systemPrompt[0] != "" {
		sp = systemPrompt[0]
	}
	return &InMemSessionManager{
		sessions:     make(map[string][]Message),
		asyncTasks:   make(map[string]map[string]AsyncTask),
		behaviors:    make(map[string]string),
		lastSumLen:   make(map[string]int),
		SystemPrompt: sp,
	}
}

// StartCleanup starts a background goroutine that evicts sessions idle for longer
// than TTL. It runs at the given interval. The goroutine stops when ctx is cancelled.
// Returns the manager for fluent chaining:
//
//	sm := history.NewInMemSessionManager("...").
//	    WithTTL(30 * time.Minute).
//	    StartCleanup(ctx, 5 * time.Minute)
//
// No-op if TTL == 0.
func (m *InMemSessionManager) StartCleanup(ctx context.Context, interval time.Duration) *InMemSessionManager {
	if m.TTL <= 0 {
		return m
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.evictExpired()
			case <-ctx.Done():
				return
			}
		}
	}()
	return m
}

// WithTTL sets the idle TTL and returns the manager for fluent chaining.
func (m *InMemSessionManager) WithTTL(ttl time.Duration) *InMemSessionManager {
	m.TTL = ttl
	return m
}

// evictExpired removes all sessions whose last access time exceeds TTL.
func (m *InMemSessionManager) evictExpired() {
	if m.TTL <= 0 {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.sessions {
		if t, ok := m.lastUse.Load(key); ok {
			if now.Sub(t.(time.Time)) > m.TTL {
				delete(m.sessions, key)
				delete(m.asyncTasks, key)
				delete(m.behaviors, key)
				delete(m.lastSumLen, key)
				m.lastUse.Delete(key)
			}
		}
	}
}

// touch records the current time as last access for a session key.
// Must be called while m.mu (read or write) is held to prevent eviction races.
func (m *InMemSessionManager) touch(sessionKey string) {
	if m.TTL > 0 {
		m.lastUse.Store(sessionKey, time.Now())
	}
}

func (m *InMemSessionManager) GetHistory(ctx context.Context, sessionKey string) []Message {
	m.mu.RLock()
	defer m.mu.RUnlock()

	m.touch(sessionKey)

	systemPrompt := m.SystemPrompt
	if behavior, ok := m.behaviors[sessionKey]; ok && behavior != "" {
		systemPrompt += "\n\n[USER BEHAVIORAL PROFILE & LONG-TERM MEMORY]: " + behavior
	}

	if history, ok := m.sessions[sessionKey]; ok {
		// Copy FIRST, then mutate the copy — never write to the shared backing array under RLock
		result := make([]Message, len(history))
		copy(result, history)
		if len(result) > 0 && result[0].Role == "system" {
			result[0].Content = systemPrompt
		}
		return result
	}

	return []Message{{Role: "system", Content: systemPrompt}}
}

// UpdateBehaviorSummary is the callback used by BackgroundBehaviorSummarizer to inject long-term memory
func (m *InMemSessionManager) UpdateBehaviorSummary(sessionKey string, newSummary string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.behaviors[sessionKey] = newSummary
	return nil
}

// SetHistory stores a copy of the messages for the given session key.
func (m *InMemSessionManager) SetHistory(_ context.Context, sessionKey string, messages []Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]Message, len(messages))
	copy(cp, messages)
	m.sessions[sessionKey] = cp
	m.touch(sessionKey)
}

func (m *InMemSessionManager) GetAsyncTasks(ctx context.Context, sessionKey string) map[string]AsyncTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.touch(sessionKey)
	tasks, ok := m.asyncTasks[sessionKey]
	if !ok {
		return map[string]AsyncTask{}
	}
	cp := make(map[string]AsyncTask, len(tasks))
	for k, v := range tasks {
		cp[k] = v
	}
	return cp
}

func (m *InMemSessionManager) SetAsyncTasks(ctx context.Context, sessionKey string, tasks map[string]AsyncTask) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]AsyncTask, len(tasks))
	for k, v := range tasks {
		cp[k] = v
	}
	m.asyncTasks[sessionKey] = cp
	m.touch(sessionKey)
}

// Fork creates a new session that is a deep copy of the first atIndex messages
// from sessionKey. The behavior summary is copied; async tasks are not.
// See SessionManager.Fork for full semantics.
func (m *InMemSessionManager) Fork(_ context.Context, sessionKey string, atIndex int) (string, error) {
	if atIndex < 0 {
		return "", fmt.Errorf("history: fork atIndex must be >= 0, got %d", atIndex)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	src, ok := m.sessions[sessionKey]
	if !ok {
		return "", fmt.Errorf("history: fork source session %q not found", sessionKey)
	}

	end := snapToSafeBoundary(src, atIndex)

	newKey, err := newForkKey(sessionKey)
	if err != nil {
		return "", err
	}

	cp := make([]Message, end)
	copy(cp, src[:end])
	m.sessions[newKey] = cp

	if behavior, ok := m.behaviors[sessionKey]; ok && behavior != "" {
		m.behaviors[newKey] = behavior
	}

	m.touch(newKey)
	return newKey, nil
}

// DeleteSession removes all in-memory state for sessionKey.
// Deleting a non-existent session is a no-op.
func (m *InMemSessionManager) DeleteSession(_ context.Context, sessionKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionKey)
	delete(m.asyncTasks, sessionKey)
	delete(m.behaviors, sessionKey)
	delete(m.lastSumLen, sessionKey)
	m.lastUse.Delete(sessionKey)
	return nil
}

func (m *InMemSessionManager) Save(_ context.Context, sessionKey string) error {
	m.mu.Lock()
	msgs := m.sessions[sessionKey]
	msgLen := len(msgs)
	lastLen := m.lastSumLen[sessionKey]

	shouldSummarize := m.SummaryProvider != nil &&
		msgLen >= backgroundSumTriggerThreshold &&
		msgLen >= lastLen+backgroundSumNewMsgThreshold

	var newMessages []Message
	var prevSummary string
	if shouldSummarize {
		m.lastSumLen[sessionKey] = msgLen
		// Only send messages since last summarization, not the full history
		start := lastLen
		if start < 0 {
			start = 0
		}
		newMessages = make([]Message, msgLen-start)
		copy(newMessages, msgs[start:])
		prevSummary = m.behaviors[sessionKey]
	}
	m.mu.Unlock()

	if shouldSummarize {
		BackgroundBehaviorSummarizer(sessionKey, newMessages, prevSummary, m.SummaryProvider, m.UpdateBehaviorSummary)
	}

	return nil
}
