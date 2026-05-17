package history

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileSessionManager provides file-backed session persistence.
// Sessions survive server restarts. Each session is stored as a JSON file under storagePath.
type FileSessionManager struct {
	sessions     map[string]*Session
	behaviors    map[string]string
	lastSumLen   map[string]int
	updatedAt    map[string]time.Time
	deletedAt    map[string]time.Time
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

// fileSessionWrapper is the persisted JSON shape on disk. Behavior /
// UpdatedAt / DeletedAt sit alongside the Session blob so they survive
// process restarts without needing a Session struct change.
type fileSessionWrapper struct {
	Session   Session    `json:"session"`
	Behavior  string     `json:"behavior,omitempty"`
	UpdatedAt time.Time  `json:"updated_at,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
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
		updatedAt:  make(map[string]time.Time),
		deletedAt:  make(map[string]time.Time),
		storage:    storagePath,
		SystemPrompt: sp,
	}, nil
}

// History returns the session history, injecting system prompt + behavior summary.
// On cache miss, it attempts to load from disk (storagePath/<sessionKey>.json).
// The error return is part of the SessionManager contract; this backend
// degrades to "empty session" on disk errors rather than propagating them,
// matching the documented best-effort semantics.
func (sm *FileSessionManager) History(_ context.Context, sessionKey string) ([]Message, error) {
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
		return result, nil
	}

	if sm.storage == "" {
		return []Message{{Role: "system", Content: systemPrompt}}, nil
	}

	data, err := os.ReadFile(filepath.Join(sm.storage, sessionKey+".json"))
	if err != nil {
		return []Message{{Role: "system", Content: systemPrompt}}, nil
	}

	var stored fileSessionWrapper
	if err := json.Unmarshal(data, &stored); err != nil {
		// legacy format: try loading as plain Session
		var legacy Session
		if err2 := json.Unmarshal(data, &legacy); err2 != nil {
			log.Printf("[FileSessionManager] failed to unmarshal %s.json: %v", sessionKey, err)
			return []Message{{Role: "system", Content: systemPrompt}}, nil
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
	if !stored.UpdatedAt.IsZero() {
		sm.updatedAt[sessionKey] = stored.UpdatedAt
	}
	if stored.DeletedAt != nil && !stored.DeletedAt.IsZero() {
		sm.deletedAt[sessionKey] = *stored.DeletedAt
	}
	sm.mu.Unlock()

	return result, nil
}

// SaveHistory atomically updates the in-memory cache and persists the wrapper
// to disk. Combines what used to be SetHistory + Save into a single atomic
// operation. Triggers background summarization when configured.
func (sm *FileSessionManager) SaveHistory(ctx context.Context, sessionKey string, msgs []Message) error {
	sm.mu.Lock()
	session, ok := sm.sessions[sessionKey]
	if !ok {
		session = &Session{Key: sessionKey}
		sm.sessions[sessionKey] = session
	}
	messages := make([]Message, len(msgs))
	copy(messages, msgs)
	session.Messages = messages
	sm.updatedAt[sessionKey] = time.Now()
	sm.mu.Unlock()
	return sm.persist(ctx, sessionKey)
}

// AsyncTasks returns a copy of the background tasks parked on sessionKey.
// Lazy-loads from disk via History when the session is not cached.
func (sm *FileSessionManager) AsyncTasks(ctx context.Context, sessionKey string) (map[string]AsyncTask, error) {
	sm.mu.RLock()
	session, ok := sm.sessions[sessionKey]
	sm.mu.RUnlock()

	if !ok {
		if _, err := sm.History(ctx, sessionKey); err != nil {
			return nil, err
		}
		sm.mu.RLock()
		session, ok = sm.sessions[sessionKey]
		sm.mu.RUnlock()
	}

	if ok && session != nil && session.AsyncTasks != nil {
		cp := make(map[string]AsyncTask, len(session.AsyncTasks))
		for k, v := range session.AsyncTasks {
			cp[k] = v
		}
		return cp, nil
	}

	return map[string]AsyncTask{}, nil
}

// SaveAsyncTasks atomically updates the in-memory cache and persists the
// wrapper to disk.
func (sm *FileSessionManager) SaveAsyncTasks(ctx context.Context, sessionKey string, tasks map[string]AsyncTask) error {
	sm.mu.Lock()
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
	sm.mu.Unlock()
	return sm.persist(ctx, sessionKey)
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
	_, _ = sm.History(ctx, sessionKey)

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

	if err := sm.persist(ctx, newKey); err != nil {
		return "", fmt.Errorf("history: fork persist %q: %w", newKey, err)
	}
	return newKey, nil
}

// Delete removes the session from the in-memory cache and deletes the backing
// JSON file (if configured). Deleting a non-existent session is a no-op.
func (sm *FileSessionManager) Delete(_ context.Context, sessionKey string) error {
	sm.mu.Lock()
	sm.purgeKeyLocked(sessionKey)
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

// purgeKeyLocked drops every map entry tied to sessionKey. Caller holds sm.mu (write).
func (sm *FileSessionManager) purgeKeyLocked(sessionKey string) {
	delete(sm.sessions, sessionKey)
	delete(sm.behaviors, sessionKey)
	delete(sm.lastSumLen, sessionKey)
	delete(sm.updatedAt, sessionKey)
	delete(sm.deletedAt, sessionKey)
}

// Query lists sessions stored on disk under storagePath. Result includes
// in-memory sessions that have not been persisted yet. See
// SessionManager.Query for full semantics. Linear in session count — for
// products with tens of thousands of sessions, prefer the MySQL backend.
func (sm *FileSessionManager) Query(_ context.Context, prefix string, opts SessionQueryOpts) ([]SessionMeta, error) {
	seen := make(map[string]bool)
	out := make([]SessionMeta, 0, 16)

	add := func(meta SessionMeta) {
		if meta.DeletedAt != nil && !opts.IncludeDeleted {
			return
		}
		out = append(out, meta)
		seen[meta.Key] = true
	}

	sm.mu.RLock()
	for key, sess := range sm.sessions {
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		var deleted *time.Time
		if dt, ok := sm.deletedAt[key]; ok && !dt.IsZero() {
			d := dt
			deleted = &d
		}
		add(SessionMeta{
			Key:          key,
			UpdatedAt:    sm.updatedAt[key],
			MessageCount: len(sess.Messages),
			DeletedAt:    deleted,
		})
	}
	sm.mu.RUnlock()

	if sm.storage != "" {
		entries, err := os.ReadDir(sm.storage)
		if err != nil {
			return nil, fmt.Errorf("history: query: read storage dir: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			key, ok := strings.CutSuffix(name, ".json")
			if !ok {
				continue
			}
			if seen[key] {
				continue
			}
			if prefix != "" && !strings.HasPrefix(key, prefix) {
				continue
			}
			meta, err := sm.readMetaFromDisk(key)
			if err != nil {
				log.Printf("[FileSessionManager] query: skip %s: %v", key, err)
				continue
			}
			add(meta)
		}
	}

	sortSessionMeta(out, opts.OrderBy)
	if opts.Offset > 0 {
		if opts.Offset >= len(out) {
			return []SessionMeta{}, nil
		}
		out = out[opts.Offset:]
	}
	if opts.Limit > 0 && opts.Limit < len(out) {
		out = out[:opts.Limit]
	}
	return out, nil
}

// readMetaFromDisk parses a session file just enough to populate SessionMeta.
func (sm *FileSessionManager) readMetaFromDisk(sessionKey string) (SessionMeta, error) {
	data, err := os.ReadFile(filepath.Join(sm.storage, sessionKey+".json"))
	if err != nil {
		return SessionMeta{}, err
	}
	var stored fileSessionWrapper
	if err := json.Unmarshal(data, &stored); err != nil {
		// legacy plain-Session shape: only Messages count is recoverable.
		var legacy Session
		if err2 := json.Unmarshal(data, &legacy); err2 != nil {
			return SessionMeta{}, err
		}
		stored.Session = legacy
	}
	meta := SessionMeta{
		Key:          sessionKey,
		UpdatedAt:    stored.UpdatedAt,
		MessageCount: len(stored.Session.Messages),
	}
	if stored.DeletedAt != nil && !stored.DeletedAt.IsZero() {
		dt := *stored.DeletedAt
		meta.DeletedAt = &dt
	}
	return meta, nil
}

// SoftDelete tombstones the session in-memory and persists the wrapper.
func (sm *FileSessionManager) SoftDelete(ctx context.Context, sessionKey string) error {
	// Ensure cache is populated so Save has something to persist.
	_, _ = sm.History(ctx, sessionKey)
	sm.mu.Lock()
	if _, ok := sm.sessions[sessionKey]; !ok {
		sm.mu.Unlock()
		return nil
	}
	if !sm.deletedAt[sessionKey].IsZero() {
		sm.mu.Unlock()
		return nil
	}
	sm.deletedAt[sessionKey] = time.Now()
	sm.mu.Unlock()
	return sm.persist(ctx, sessionKey)
}

// Restore clears the tombstone in-memory and persists the wrapper.
func (sm *FileSessionManager) Restore(ctx context.Context, sessionKey string) error {
	_, _ = sm.History(ctx, sessionKey)
	sm.mu.Lock()
	if _, ok := sm.sessions[sessionKey]; !ok {
		sm.mu.Unlock()
		return nil
	}
	delete(sm.deletedAt, sessionKey)
	sm.mu.Unlock()
	return sm.persist(ctx, sessionKey)
}

// PurgeDeletedBefore hard-deletes every soft-deleted session whose
// DeletedAt is strictly older than `before`.
func (sm *FileSessionManager) PurgeDeletedBefore(ctx context.Context, before time.Time) (int, error) {
	metas, err := sm.Query(ctx, "", SessionQueryOpts{IncludeDeleted: true})
	if err != nil {
		return 0, err
	}
	purged := 0
	for _, m := range metas {
		if m.DeletedAt == nil || !m.DeletedAt.Before(before) {
			continue
		}
		if err := sm.Delete(ctx, m.Key); err != nil {
			return purged, err
		}
		purged++
	}
	return purged, nil
}

// persist writes the in-memory wrapper to disk and triggers background
// summarization if conditions are met. Internal helper invoked by
// SaveHistory, SaveAsyncTasks, SoftDelete, Restore, and Fork — the
// SessionManager interface no longer exposes a standalone Save.
func (sm *FileSessionManager) persist(_ context.Context, sessionKey string) error {
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

	sm.mu.RLock()
	updatedAt := sm.updatedAt[sessionKey]
	deletedAt := sm.deletedAt[sessionKey]
	sm.mu.RUnlock()
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	stored := fileSessionWrapper{Session: snapshot, Behavior: behavior, UpdatedAt: updatedAt}
	if !deletedAt.IsZero() {
		dt := deletedAt
		stored.DeletedAt = &dt
	}

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
