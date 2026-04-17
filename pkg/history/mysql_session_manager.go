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
// History survives server restarts. Optionally supports background behavior summarization.
type MySQLSessionManager struct {
	db           *sql.DB
	sessions     map[string]*Session
	behaviors    map[string]string
	lastSumLen   map[string]int
	mu           sync.RWMutex
	SystemPrompt string
	SummaryProvider SummaryProvider // if nil, background summarization is disabled
}

// NewMySQLSessionManager creates a MySQL-backed session manager.
// An optional systemPrompt can be provided; defaults to a generic assistant prompt.
func NewMySQLSessionManager(db *sql.DB, systemPrompt ...string) (*MySQLSessionManager, error) {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_sessions (
			session_key VARCHAR(255) PRIMARY KEY,
			messages    JSON NOT NULL,
			behavior    TEXT,
			async_tasks JSON,
			updated_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		)
	`); err != nil {
		return nil, fmt.Errorf("failed to create session table: %w", err)
	}

	// Add async_tasks column if it doesn't exist (backward compatibility filter)
	db.Exec("ALTER TABLE agent_sessions ADD COLUMN async_tasks JSON")

	sp := "You are an AI assistant."
	if len(systemPrompt) > 0 && systemPrompt[0] != "" {
		sp = systemPrompt[0]
	}
	return &MySQLSessionManager{
		db:           db,
		sessions:     make(map[string]*Session),
		behaviors:    make(map[string]string),
		lastSumLen:   make(map[string]int),
		SystemPrompt: sp,
	}, nil
}

func (sm *MySQLSessionManager) GetHistory(ctx context.Context, sessionKey string) []Message {
	sm.mu.RLock()
	session, ok := sm.sessions[sessionKey]
	behavior := sm.behaviors[sessionKey]
	sm.mu.RUnlock()

	systemPrompt := sm.SystemPrompt
	if behavior != "" {
		systemPrompt += "\n\n[USER BEHAVIORAL PROFILE & LONG-TERM MEMORY]: " + behavior
	}

	if ok {
		result := make([]Message, len(session.Messages))
		copy(result, session.Messages)
		if len(result) > 0 && result[0].Role == "system" {
			result[0].Content = systemPrompt
		}
		return result
	}

	// Load from MySQL
	var messagesJSON string
	var behaviorSQL sql.NullString
	err := sm.db.QueryRowContext(ctx,
		"SELECT messages, behavior FROM agent_sessions WHERE session_key = ?", sessionKey,
	).Scan(&messagesJSON, &behaviorSQL)

	if err == sql.ErrNoRows {
		return []Message{{Role: "system", Content: systemPrompt}}
	} else if err != nil {
		log.Printf("[MySQLSessionManager] GetHistory error for %q: %v", sessionKey, err)
		return []Message{{Role: "system", Content: systemPrompt}}
	}

	var msgs []Message
	if err := json.Unmarshal([]byte(messagesJSON), &msgs); err != nil {
		log.Printf("[MySQLSessionManager] unmarshal error for %q: %v", sessionKey, err)
		return []Message{{Role: "system", Content: systemPrompt}}
	}

	if len(msgs) > 0 && msgs[0].Role == "system" {
		msgs[0].Content = systemPrompt
	}

	// Restore behavior summary from DB
	if behaviorSQL.Valid && behaviorSQL.String != "" {
		sm.mu.Lock()
		sm.behaviors[sessionKey] = behaviorSQL.String
		sm.mu.Unlock()
	}

	result := make([]Message, len(msgs))
	copy(result, msgs)

	sm.mu.Lock()
	sm.sessions[sessionKey] = &Session{Key: sessionKey, Messages: msgs}
	sm.mu.Unlock()

	return result
}

// SetHistory stores a copy of the messages for the given session key (in-memory cache only).
func (sm *MySQLSessionManager) SetHistory(_ context.Context, sessionKey string, messages []Message) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	session, ok := sm.sessions[sessionKey]
	if !ok {
		session = &Session{Key: sessionKey}
		sm.sessions[sessionKey] = session
	}
	cp := make([]Message, len(messages))
	copy(cp, messages)
	session.Messages = cp
}

func (sm *MySQLSessionManager) GetAsyncTasks(ctx context.Context, sessionKey string) map[string]AsyncTask {
	sm.mu.RLock()
	session, ok := sm.sessions[sessionKey]
	sm.mu.RUnlock()

	if ok && session.AsyncTasks != nil {
		cp := make(map[string]AsyncTask, len(session.AsyncTasks))
		for k, v := range session.AsyncTasks {
			cp[k] = v
		}
		return cp
	}

	var asyncTasksJSON sql.NullString
	err := sm.db.QueryRowContext(ctx, "SELECT async_tasks FROM agent_sessions WHERE session_key = ?", sessionKey).Scan(&asyncTasksJSON)
	if err == nil && asyncTasksJSON.Valid && asyncTasksJSON.String != "" {
		var tasks map[string]AsyncTask
		if json.Unmarshal([]byte(asyncTasksJSON.String), &tasks) == nil {
			sm.mu.Lock()
			if _, ok := sm.sessions[sessionKey]; !ok {
				sm.sessions[sessionKey] = &Session{Key: sessionKey}
			}
			sm.sessions[sessionKey].AsyncTasks = tasks
			sm.mu.Unlock()

			cp := make(map[string]AsyncTask, len(tasks))
			for k, v := range tasks {
				cp[k] = v
			}
			return cp
		}
	}
	return map[string]AsyncTask{}
}

func (sm *MySQLSessionManager) SetAsyncTasks(ctx context.Context, sessionKey string, tasks map[string]AsyncTask) {
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
func (sm *MySQLSessionManager) UpdateBehaviorSummary(sessionKey string, newSummary string) error {
	sm.mu.Lock()
	sm.behaviors[sessionKey] = newSummary
	sm.mu.Unlock()
	// Will be persisted on next Save()
	return nil
}

// DeleteSession removes the session from the in-memory cache and deletes the
// corresponding row from the agent_sessions table. Deleting a non-existent
// session is a no-op.
func (sm *MySQLSessionManager) DeleteSession(ctx context.Context, sessionKey string) error {
	sm.mu.Lock()
	delete(sm.sessions, sessionKey)
	delete(sm.behaviors, sessionKey)
	delete(sm.lastSumLen, sessionKey)
	sm.mu.Unlock()

	if _, err := sm.db.ExecContext(ctx, "DELETE FROM agent_sessions WHERE session_key = ?", sessionKey); err != nil {
		return fmt.Errorf("history: delete session %q: %w", sessionKey, err)
	}
	return nil
}

func (sm *MySQLSessionManager) Save(ctx context.Context, sessionKey string) error {
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
	cp := make([]Message, msgLen)
	copy(cp, session.Messages)
	behavior := sm.behaviors[sessionKey]
	sm.mu.Unlock()

	if shouldSummarize {
		BackgroundBehaviorSummarizer(sessionKey, newMessages, prevSummary, sm.SummaryProvider, sm.UpdateBehaviorSummary)
	}

	msgsBytes, err := json.Marshal(cp)
	if err != nil {
		return err
	}

	asyncTasksBytes, _ := json.Marshal(session.AsyncTasks)

	query := `
		INSERT INTO agent_sessions (session_key, messages, behavior, async_tasks)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE messages = VALUES(messages), behavior = VALUES(behavior), async_tasks = VALUES(async_tasks)
	`
	_, err = sm.db.ExecContext(ctx, query, sessionKey, string(msgsBytes), behavior, string(asyncTasksBytes))
	return err
}
