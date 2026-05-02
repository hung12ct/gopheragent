package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultMySQLMessagesTableName is the table used by
// MySQLAppendOnlyMessageStore when no override is provided.
const DefaultMySQLMessagesTableName = "agent_messages"

// MySQLAppendOnlyMessageStore stores each message as its own row keyed by
// (session_key, idx). Per-turn writes touch only the new rows, sidestepping
// the JSON-column row-size cap that bites long sessions when messages live
// inside a single blob.
//
// Schema:
//
//	CREATE TABLE agent_messages (
//	    session_key VARCHAR(255) NOT NULL,
//	    idx         INT UNSIGNED NOT NULL,
//	    payload     JSON NOT NULL,
//	    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
//	    PRIMARY KEY (session_key, idx),
//	    INDEX idx_session_created (session_key, created_at)
//	)
//
// The store is safe for concurrent calls but does NOT lock per-session —
// callers should serialize Save calls per-session if they need strict
// ordering across goroutines (the SessionManager already does).
type MySQLAppendOnlyMessageStore struct {
	db        *sql.DB
	tableName string
}

// NewMySQLAppendOnlyMessageStore constructs the store and runs the
// idempotent CREATE TABLE statement. tableName is validated against
// SQL identifier rules; pass "" to use DefaultMySQLMessagesTableName.
func NewMySQLAppendOnlyMessageStore(db *sql.DB, tableName string) (*MySQLAppendOnlyMessageStore, error) {
	if tableName == "" {
		tableName = DefaultMySQLMessagesTableName
	}
	if !mysqlIdentRE.MatchString(tableName) {
		return nil, fmt.Errorf("history: invalid messages table name %q", tableName)
	}
	createStmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			session_key VARCHAR(255) NOT NULL,
			idx         INT UNSIGNED NOT NULL,
			payload     JSON NOT NULL,
			created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_key, idx),
			INDEX idx_session_created (session_key, created_at)
		)`, tableName)
	if _, err := db.Exec(createStmt); err != nil {
		return nil, fmt.Errorf("history: create messages table: %w", err)
	}
	return &MySQLAppendOnlyMessageStore{db: db, tableName: tableName}, nil
}

// Save writes messages[knownPersistedCount:] as new rows, then UPDATEs the
// last already-persisted row to absorb any in-place mutation (LLM
// streaming attaches tool_calls to an assistant message after the row is
// first created — without the upsert that edit would be lost).
// Implements truncation: when len(messages) < knownPersistedCount the
// orphan rows at the tail are deleted.
func (s *MySQLAppendOnlyMessageStore) Save(ctx context.Context, sessionKey string, messages []Message, knownPersistedCount int) (int, error) {
	cur := len(messages)

	if cur < knownPersistedCount {
		// Truncation — delete orphan tail rows.
		_, err := s.db.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE session_key = ? AND idx >= ?", s.tableName),
			sessionKey, cur,
		)
		if err != nil {
			return knownPersistedCount, fmt.Errorf("history: truncate tail: %w", err)
		}
		// Fall through to upsert the kept range so any in-place edits
		// (CacheHint flips, content rewrites) land on disk.
		knownPersistedCount = 0
	}

	if cur == 0 {
		return 0, nil
	}

	// Upsert: any rows from idx 0 to cur-1 that already exist get refreshed,
	// new rows get inserted. ON DUPLICATE KEY UPDATE handles the upsert.
	// Build a single multi-row INSERT to keep round-trips O(1).
	if knownPersistedCount > 0 && knownPersistedCount < cur {
		// Refresh just the last already-persisted row (cheap), then insert
		// the new tail. Tools-call edits typically modify only the most
		// recent assistant message, so this avoids touching cold rows.
		if err := s.upsertOne(ctx, sessionKey, knownPersistedCount-1, messages[knownPersistedCount-1]); err != nil {
			return knownPersistedCount, err
		}
		return cur, s.insertRange(ctx, sessionKey, knownPersistedCount, messages[knownPersistedCount:])
	}
	// Either fresh save (knownCount==0) or no new messages (knownCount==cur).
	// In both cases write everything we have via upsert.
	return cur, s.upsertRange(ctx, sessionKey, 0, messages)
}

func (s *MySQLAppendOnlyMessageStore) upsertOne(ctx context.Context, key string, idx int, m Message) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("history: marshal msg %d: %w", idx, err)
	}
	q := fmt.Sprintf(`INSERT INTO %s (session_key, idx, payload) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE payload = VALUES(payload)`, s.tableName)
	if _, err := s.db.ExecContext(ctx, q, key, idx, string(payload)); err != nil {
		return fmt.Errorf("history: upsert msg %d: %w", idx, err)
	}
	return nil
}

func (s *MySQLAppendOnlyMessageStore) insertRange(ctx context.Context, key string, baseIdx int, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(messages))
	args := make([]any, 0, len(messages)*3)
	for i, m := range messages {
		payload, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("history: marshal msg %d: %w", baseIdx+i, err)
		}
		placeholders = append(placeholders, "(?, ?, ?)")
		args = append(args, key, baseIdx+i, string(payload))
	}
	q := fmt.Sprintf(`INSERT INTO %s (session_key, idx, payload) VALUES %s
		ON DUPLICATE KEY UPDATE payload = VALUES(payload)`,
		s.tableName, strings.Join(placeholders, ","))
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("history: insert messages: %w", err)
	}
	return nil
}

// upsertRange is identical to insertRange but kept as a distinct verb at
// the call site for clarity ("we're rewriting everything we have").
func (s *MySQLAppendOnlyMessageStore) upsertRange(ctx context.Context, key string, baseIdx int, messages []Message) error {
	return s.insertRange(ctx, key, baseIdx, messages)
}

// Load returns every message for sessionKey, ordered by idx.
func (s *MySQLAppendOnlyMessageStore) Load(ctx context.Context, sessionKey string) ([]Message, error) {
	q := fmt.Sprintf("SELECT payload FROM %s WHERE session_key = ? ORDER BY idx ASC", s.tableName)
	rows, err := s.db.QueryContext(ctx, q, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("history: load messages: %w", err)
	}
	defer rows.Close()
	out := make([]Message, 0, 16)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("history: scan message row: %w", err)
		}
		var m Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("history: unmarshal message: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: iterate messages: %w", err)
	}
	return out, nil
}

// Delete removes every row for sessionKey.
func (s *MySQLAppendOnlyMessageStore) Delete(ctx context.Context, sessionKey string) error {
	_, err := s.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE session_key = ?", s.tableName), sessionKey)
	if err != nil {
		return fmt.Errorf("history: delete messages: %w", err)
	}
	return nil
}
