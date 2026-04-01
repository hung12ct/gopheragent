package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
)

// MySQLSessionManager implements SessionManager using MySQL for persistence.
type MySQLSessionManager struct {
	db       *sql.DB
	sessions map[string]*Session
	mu       sync.RWMutex
}

func NewMySQLSessionManager(db *sql.DB) (*MySQLSessionManager, error) {
	query := `
		CREATE TABLE IF NOT EXISTS agent_sessions (
			session_key VARCHAR(255) PRIMARY KEY,
			messages JSON NOT NULL,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		)
	`
	if _, err := db.Exec(query); err != nil {
		return nil, fmt.Errorf("failed to create session table: %w", err)
	}
	return &MySQLSessionManager{
		db:       db,
		sessions: make(map[string]*Session),
	}, nil
}

func (sm *MySQLSessionManager) GetHistory(ctx context.Context, sessionKey string) []Message {
	sm.mu.RLock()
	session, ok := sm.sessions[sessionKey]
	if ok {
		// Copy under lock to prevent race with SetHistory
		result := make([]Message, len(session.Messages))
		copy(result, session.Messages)
		sm.mu.RUnlock()
		return result
	}
	sm.mu.RUnlock()

	var messagesJSON string
	err := sm.db.QueryRowContext(ctx, "SELECT messages FROM agent_sessions WHERE session_key = ?", sessionKey).Scan(&messagesJSON)

	if err == sql.ErrNoRows {
		return []Message{}
	} else if err != nil {
		log.Printf("[MySQLSessionManager] GetHistory query error for session %q: %v", sessionKey, err)
		return []Message{}
	}

	var msgs []Message
	if err := json.Unmarshal([]byte(messagesJSON), &msgs); err != nil {
		log.Printf("[MySQLSessionManager] GetHistory unmarshal error for session %q: %v", sessionKey, err)
		return []Message{}
	}

	// Return a copy — the cache owns its own slice
	result := make([]Message, len(msgs))
	copy(result, msgs)

	sm.mu.Lock()
	sm.sessions[sessionKey] = &Session{Key: sessionKey, Messages: msgs}
	sm.mu.Unlock()

	return result
}

// SetHistory stores a copy of the messages for the given session key.
func (sm *MySQLSessionManager) SetHistory(_ context.Context, sessionKey string, history []Message) {
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

func (sm *MySQLSessionManager) Save(ctx context.Context, sessionKey string) error {
	sm.mu.RLock()
	session, ok := sm.sessions[sessionKey]
	if !ok {
		sm.mu.RUnlock()
		return nil
	}
	cp := make([]Message, len(session.Messages))
	copy(cp, session.Messages)
	sm.mu.RUnlock()

	msgsBytes, err := json.Marshal(cp)

	if err != nil {
		return err
	}

	query := `
		INSERT INTO agent_sessions (session_key, messages) 
		VALUES (?, ?) 
		ON DUPLICATE KEY UPDATE messages = ?
	`
	_, err = sm.db.ExecContext(ctx, query, sessionKey, string(msgsBytes), string(msgsBytes))
	return err
}
