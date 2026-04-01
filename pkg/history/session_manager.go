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
// Sessions are cached in-memory after first load and lazily hydrated from disk
// when GetHistory is called for an unknown session key.
type FileSessionManager struct {
	sessions map[string]*Session
	storage  string
	mu       sync.RWMutex
}

// NewFileSessionManager creates a file-backed session manager.
// storagePath is the directory where session JSON files are stored.
func NewFileSessionManager(storagePath string) (*FileSessionManager, error) {
	if storagePath != "" {
		if err := os.MkdirAll(storagePath, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create session storage directory: %w", err)
		}
	}
	return &FileSessionManager{
		sessions: make(map[string]*Session),
		storage:  storagePath,
	}, nil
}

// GetHistory returns a copy of the session messages.
// On cache miss, it attempts to load from disk (storagePath/<sessionKey>.json).
func (sm *FileSessionManager) GetHistory(_ context.Context, sessionKey string) []Message {
	sm.mu.RLock()
	session, ok := sm.sessions[sessionKey]
	if ok {
		result := make([]Message, len(session.Messages))
		copy(result, session.Messages)
		sm.mu.RUnlock()
		return result
	}
	sm.mu.RUnlock()

	if sm.storage == "" {
		return []Message{}
	}

	data, err := os.ReadFile(filepath.Join(sm.storage, sessionKey+".json"))
	if err != nil {
		return []Message{}
	}
	var loaded Session
	if err := json.Unmarshal(data, &loaded); err != nil {
		log.Printf("[FileSessionManager] failed to unmarshal %s.json: %v", sessionKey, err)
		return []Message{}
	}

	result := make([]Message, len(loaded.Messages))
	copy(result, loaded.Messages)

	sm.mu.Lock()
	sm.sessions[sessionKey] = &loaded
	sm.mu.Unlock()

	return result
}

// SetHistory stores a copy of the messages for the given session key.
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

func (sm *FileSessionManager) Save(_ context.Context, sessionKey string) error {
	if sm.storage == "" {
		return nil
	}

	sm.mu.RLock()
	stored, ok := sm.sessions[sessionKey]
	if !ok {
		sm.mu.RUnlock()
		return nil
	}
	// Copy + marshal under lock to prevent race with SetHistory
	snapshot := Session{Key: stored.Key, Messages: make([]Message, len(stored.Messages))}
	copy(snapshot.Messages, stored.Messages)
	sm.mu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
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

	sessionPath := filepath.Join(sm.storage, sessionKey+".json")
	return os.Rename(tmpPath, sessionPath)
}
