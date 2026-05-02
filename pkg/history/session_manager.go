package history

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// FileSessionManager provides file-backed session persistence.
// Sessions survive server restarts. Each session is stored as a JSON file under storagePath.
type FileSessionManager struct {
	sessions     map[string]*Session
	behaviors    map[string]string
	lastSumLen   map[string]int
	storage      string
	mu           sync.RWMutex
	SystemPrompt    string
	SummaryProvider SummaryProvider // if nil, background summarization is disabled
	// PromptVersion: see InMemSessionManager.PromptVersion. Same semantics.
	PromptVersion string
}

// WithPromptVersion sets the prompt version tag and returns the manager
// for fluent chaining.
func (sm *FileSessionManager) WithPromptVersion(version string) *FileSessionManager {
	sm.PromptVersion = version
	return sm
}

// NewFileSessionManager creates a file-backed session manager.
// storagePath is the directory where session JSON files are stored.
// An optional systemPrompt can be provided; defaults to a generic assistant prompt.
func NewFileSessionManager(storagePath string, systemPrompt ...string) (*FileSessionManager, error) {
	if storagePath != "" {
		if err := os.MkdirAll(storagePath, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create session storage directory: %w", err)
		}
	}
	sp := "You are an AI assistant."
	if len(systemPrompt) > 0 && systemPrompt[0] != "" {
		sp = systemPrompt[0]
	}
	return &FileSessionManager{
		sessions:   make(map[string]*Session),
		behaviors:  make(map[string]string),
		lastSumLen: make(map[string]int),
		storage:    storagePath,
		SystemPrompt: sp,
	}, nil
}

// GetHistory returns the session history, injecting system prompt + behavior summary.
// On cache miss, it attempts to load from disk (storagePath/<sessionKey>.json).
func (sm *FileSessionManager) GetHistory(_ context.Context, sessionKey string) []Message {
	sm.mu.RLock()
	session, ok := sm.sessions[sessionKey]
	behavior := sm.behaviors[sessionKey]
	sm.mu.RUnlock()

	systemPrompt := sm.SystemPrompt
	if behavior != "" {
		systemPrompt += "\n\n[USER BEHAVIORAL PROFILE & LONG-TERM MEMORY]: " + behavior
	}
	systemPrompt = stampPromptVersion(sm.PromptVersion, systemPrompt)

	if ok {
		result := make([]Message, len(session.Messages))
		copy(result, session.Messages)
		if len(result) > 0 && result[0].Role == "system" {
			result[0].Content = systemPrompt
		}
		return result
	}

	if sm.storage == "" {
		return []Message{{Role: "system", Content: systemPrompt}}
	}

	data, err := os.ReadFile(filepath.Join(sm.storage, sessionKey+".json"))
	if err != nil {
		return []Message{{Role: "system", Content: systemPrompt}}
	}

	var stored struct {
		Session  Session `json:"session"`
		Behavior string  `json:"behavior"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		// legacy format: try loading as plain Session
		var legacy Session
		if err2 := json.Unmarshal(data, &legacy); err2 != nil {
			log.Printf("[FileSessionManager] failed to unmarshal %s.json: %v", sessionKey, err)
			return []Message{{Role: "system", Content: systemPrompt}}
		}
		stored.Session = legacy
	}

	if stored.Behavior != "" {
		sm.mu.Lock()
		sm.behaviors[sessionKey] = stored.Behavior
		sm.mu.Unlock()
		if stored.Behavior != "" {
			systemPrompt = stampPromptVersion(sm.PromptVersion,
				sm.SystemPrompt+"\n\n[USER BEHAVIORAL PROFILE & LONG-TERM MEMORY]: "+stored.Behavior)
		}
	}

	if len(stored.Session.Messages) > 0 && stored.Session.Messages[0].Role == "system" {
		stored.Session.Messages[0].Content = systemPrompt
	}

	result := make([]Message, len(stored.Session.Messages))
	copy(result, stored.Session.Messages)

	sm.mu.Lock()
	sm.sessions[sessionKey] = &stored.Session
	sm.mu.Unlock()

	return result
}

// SetHistory stores a copy of the messages for the given session key (in-memory cache only).
func (sm *FileSessionManager) SetHistory(_ context.Context, sessionKey string, history []Message) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[sessionKey]
	if !ok {
		session = &Session{Key: sessionKey}
		sm.sessions[sessionKey] = session
	}
	messages := make([]Message, len(history))
	copy(messages, history)
	session.Messages = messages
}

func (sm *FileSessionManager) GetAsyncTasks(ctx context.Context, sessionKey string) map[string]AsyncTask {
	sm.mu.RLock()
	session, ok := sm.sessions[sessionKey]
	sm.mu.RUnlock()

	if !ok {
		// Just call GetHistory to load the full session into memory cache
		sm.GetHistory(ctx, sessionKey)
		sm.mu.RLock()
		session, ok = sm.sessions[sessionKey]
		sm.mu.RUnlock()
	}

	if ok && session != nil && session.AsyncTasks != nil {
		cp := make(map[string]AsyncTask, len(session.AsyncTasks))
		for k, v := range session.AsyncTasks {
			cp[k] = v
		}
		return cp
	}

	return map[string]AsyncTask{}
}

func (sm *FileSessionManager) SetAsyncTasks(ctx context.Context, sessionKey string, tasks map[string]AsyncTask) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	session, ok := sm.sessions[sessionKey]
	if !ok {
		session = &Session{Key: sessionKey}
		sm.sessions[sessionKey] = session
	}
	cp := make(map[string]AsyncTask, len(tasks))
	for k, v := range tasks {
		cp[k] = v
	}
	session.AsyncTasks = cp
}

// UpdateBehaviorSummary is the callback used by BackgroundBehaviorSummarizer.
func (sm *FileSessionManager) UpdateBehaviorSummary(sessionKey string, newSummary string) error {
	sm.mu.Lock()
	sm.behaviors[sessionKey] = newSummary
	sm.mu.Unlock()
	return nil
}

// Fork creates a new session whose message history is a copy of the first
// atIndex messages from sessionKey. The copy is persisted to disk (when a
// storage path is configured). See SessionManager.Fork for full semantics.
func (sm *FileSessionManager) Fork(ctx context.Context, sessionKey string, atIndex int) (string, error) {
	if atIndex < 0 {
		return "", fmt.Errorf("history: fork atIndex must be >= 0, got %d", atIndex)
	}

	// Ensure the source is loaded into the cache.
	sm.GetHistory(ctx, sessionKey)

	sm.mu.Lock()
	src, ok := sm.sessions[sessionKey]
	if !ok {
		sm.mu.Unlock()
		return "", fmt.Errorf("history: fork source session %q not found", sessionKey)
	}

	end := snapToSafeBoundary(src.Messages, atIndex)

	newKey, err := newForkKey(sessionKey)
	if err != nil {
		sm.mu.Unlock()
		return "", err
	}

	cp := make([]Message, end)
	copy(cp, src.Messages[:end])
	sm.sessions[newKey] = &Session{Key: newKey, Messages: cp}

	if behavior, ok := sm.behaviors[sessionKey]; ok && behavior != "" {
		sm.behaviors[newKey] = behavior
	}
	sm.mu.Unlock()

	if err := sm.Save(ctx, newKey); err != nil {
		return "", fmt.Errorf("history: fork persist %q: %w", newKey, err)
	}
	return newKey, nil
}

// DeleteSession removes the session from the in-memory cache and deletes the
// backing JSON file (if configured). Deleting a non-existent session is a no-op.
func (sm *FileSessionManager) DeleteSession(_ context.Context, sessionKey string) error {
	sm.mu.Lock()
	delete(sm.sessions, sessionKey)
	delete(sm.behaviors, sessionKey)
	delete(sm.lastSumLen, sessionKey)
	sm.mu.Unlock()

	if sm.storage == "" {
		return nil
	}
	err := os.Remove(filepath.Join(sm.storage, sessionKey+".json"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("history: delete session %q: %w", sessionKey, err)
	}
	return nil
}

// Save persists the session to disk and triggers background summarization if conditions met.
func (sm *FileSessionManager) Save(ctx context.Context, sessionKey string) error {
	sm.mu.Lock()
	session, ok := sm.sessions[sessionKey]
	if !ok {
		sm.mu.Unlock()
		return nil
	}
	msgLen := len(session.Messages)
	lastLen := sm.lastSumLen[sessionKey]
	shouldSummarize := sm.SummaryProvider != nil &&
		msgLen >= backgroundSumTriggerThreshold &&
		msgLen >= lastLen+backgroundSumNewMsgThreshold

	var newMessages []Message
	var prevSummary string
	if shouldSummarize {
		sm.lastSumLen[sessionKey] = msgLen
		start := lastLen
		if start < 0 {
			start = 0
		}
		newMessages = make([]Message, msgLen-start)
		copy(newMessages, session.Messages[start:])
		prevSummary = sm.behaviors[sessionKey]
	}
	snapshot := Session{Key: session.Key, Messages: make([]Message, msgLen)}
	copy(snapshot.Messages, session.Messages)
	behavior := sm.behaviors[sessionKey]
	sm.mu.Unlock()

	if shouldSummarize {
		BackgroundBehaviorSummarizer(sessionKey, newMessages, prevSummary, sm.SummaryProvider, sm.UpdateBehaviorSummary)
	}

	if sm.storage == "" {
		return nil
	}

	stored := struct {
		Session  Session `json:"session"`
		Behavior string  `json:"behavior,omitempty"`
	}{Session: snapshot, Behavior: behavior}

	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(sm.storage, "session-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return os.Rename(tmpPath, filepath.Join(sm.storage, sessionKey+".json"))
}
