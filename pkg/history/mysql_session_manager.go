package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultMySQLTableName is the table name used when no WithMySQLTableName
// option is supplied to NewMySQLSessionManagerWithOptions.
const DefaultMySQLTableName = "agent_sessions"

// mysqlIdentRE validates MySQL identifiers passed as table names. Using a
// whitelist prevents SQL injection since table names cannot use ? bindings.
var mysqlIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// MySQLOption configures a MySQLSessionManager at construction time.
type MySQLOption func(*mysqlOptions)

type mysqlOptions struct {
	tableName     string
	promptVersion string
	messageStore  MessageStore
}

// WithMySQLMessageStore plugs an alternate MessageStore into the manager.
// When set, messages are persisted via the store (typically a
// MySQLAppendOnlyMessageStore) instead of the JSON column. The session row
// still carries behavior summary, async tasks, and timestamps.
func WithMySQLMessageStore(store MessageStore) MySQLOption {
	return func(o *mysqlOptions) { o.messageStore = store }
}

// WithMySQLTableName overrides the default "agent_sessions" table name.
// Useful when multiple agents share a single database and need isolated
// tables (e.g. "chatbot_sessions", "boa_sessions"). The name must match
// the standard SQL identifier format: [A-Za-z_][A-Za-z0-9_]*.
func WithMySQLTableName(name string) MySQLOption {
	return func(o *mysqlOptions) { o.tableName = name }
}

// MySQLSessionManager implements SessionManager using MySQL for persistence.
// History survives server restarts. Optionally supports background behavior summarization.
type MySQLSessionManager struct {
	db              *sql.DB
	tableName       string
	sessions        map[string]*Session
	behaviors       map[string]string
	lastSumLen      map[string]int
	persistedCount  map[string]int // messages already on disk via MessageStore (per-session hint)
	mu              sync.RWMutex
	SystemPrompt    string
	SummaryProvider SummaryProvider // if nil, background summarization is disabled
	// PromptVersion: see InMemSessionManager.PromptVersion. Same semantics.
	PromptVersion string
	// MessageStore, when non-nil, replaces the JSON-column message
	// persistence path with per-row storage. See WithMySQLMessageStore.
	MessageStore MessageStore
}

// WithMySQLPromptVersion is the option-style constructor variant for setting
// the prompt version tag at construction. Pass to NewMySQLSessionManagerWithOptions.
func WithMySQLPromptVersion(version string) MySQLOption {
	return func(o *mysqlOptions) { o.promptVersion = version }
}

// NewMySQLSessionManager creates a MySQL-backed session manager using the
// default table name "agent_sessions". For multi-tenant deployments where
// multiple agents share one database, use NewMySQLSessionManagerWithOptions
// with WithMySQLTableName.
// An optional systemPrompt can be provided; defaults to a generic assistant prompt.
func NewMySQLSessionManager(db *sql.DB, systemPrompt ...string) (*MySQLSessionManager, error) {
	sp := ""
	if len(systemPrompt) > 0 {
		sp = systemPrompt[0]
	}
	return NewMySQLSessionManagerWithOptions(db, sp)
}

// NewMySQLSessionManagerWithOptions creates a MySQL-backed session manager
// with explicit configuration options. Pass WithMySQLTableName to override
// the default table name.
func NewMySQLSessionManagerWithOptions(db *sql.DB, systemPrompt string, opts ...MySQLOption) (*MySQLSessionManager, error) {
	cfg := mysqlOptions{tableName: DefaultMySQLTableName}
	for _, opt := range opts {
		opt(&cfg)
	}
	if !mysqlIdentRE.MatchString(cfg.tableName) {
		return nil, fmt.Errorf("history: invalid MySQL table name %q (must match [A-Za-z_][A-Za-z0-9_]*)", cfg.tableName)
	}

	createStmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			session_key VARCHAR(255) PRIMARY KEY,
			messages    JSON NOT NULL,
			behavior    TEXT,
			async_tasks JSON,
			updated_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			deleted_at  TIMESTAMP NULL DEFAULT NULL,
			INDEX idx_deleted_at (deleted_at),
			INDEX idx_updated_at (updated_at)
		)`, cfg.tableName)
	if _, err := db.Exec(createStmt); err != nil {
		return nil, fmt.Errorf("failed to create session table: %w", err)
	}
	// Idempotent migration for tables created by older versions that
	// predate the deleted_at column. Duplicate-column (1060) is benign.
	addStmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN deleted_at TIMESTAMP NULL DEFAULT NULL", cfg.tableName)
	if _, err := db.Exec(addStmt); err != nil && !isMySQLDuplicateColumn(err) {
		return nil, fmt.Errorf("failed to add deleted_at column: %w", err)
	}
	for _, idx := range []struct{ name, cols string }{
		{"idx_deleted_at", "(deleted_at)"},
		// idx_updated_at backs the sidebar query
		// `WHERE session_key LIKE ? ORDER BY updated_at DESC LIMIT N`
		// which would otherwise filesort over every prefix-matching row.
		{"idx_updated_at", "(updated_at)"},
	} {
		idxStmt := fmt.Sprintf("ALTER TABLE %s ADD INDEX %s %s", cfg.tableName, idx.name, idx.cols)
		if _, err := db.Exec(idxStmt); err != nil && !isMySQLDuplicateKey(err) {
			log.Printf("[MySQLSessionManager] add %s: %v", idx.name, err)
		}
	}

	sp := "You are an AI assistant."
	if systemPrompt != "" {
		sp = systemPrompt
	}
	return &MySQLSessionManager{
		db:             db,
		tableName:      cfg.tableName,
		sessions:       make(map[string]*Session),
		behaviors:      make(map[string]string),
		lastSumLen:     make(map[string]int),
		persistedCount: make(map[string]int),
		SystemPrompt:   sp,
		PromptVersion:  cfg.promptVersion,
		MessageStore:   cfg.messageStore,
	}, nil
}

// History returns the persisted message log for sessionKey. Reads from the
// in-memory cache when present; otherwise loads from MySQL (or the
// MessageStore when configured). Disk/decoding errors are logged and the
// method degrades to "system-only" history rather than failing the loop —
// the error return is reserved for unrecoverable infrastructure errors
// that callers should propagate.
func (sm *MySQLSessionManager) History(ctx context.Context, sessionKey string) ([]Message, error) {
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

	// Load from MySQL — when MessageStore is set, messages live in a
	// separate per-row table; the session row only carries metadata.
	var messagesJSON string
	var behaviorSQL sql.NullString
	err := sm.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT messages, behavior FROM %s WHERE session_key = ?", sm.tableName), sessionKey,
	).Scan(&messagesJSON, &behaviorSQL)

	if err == sql.ErrNoRows {
		return []Message{{Role: "system", Content: systemPrompt}}, nil
	} else if err != nil {
		log.Printf("[MySQLSessionManager] History error for %q: %v", sessionKey, err)
		return []Message{{Role: "system", Content: systemPrompt}}, nil
	}

	var msgs []Message
	if sm.MessageStore != nil {
		loaded, lerr := sm.MessageStore.Load(ctx, sessionKey)
		if lerr != nil {
			log.Printf("[MySQLSessionManager] MessageStore.Load %q: %v", sessionKey, lerr)
			return []Message{{Role: "system", Content: systemPrompt}}, nil
		}
		msgs = loaded
		sm.mu.Lock()
		sm.persistedCount[sessionKey] = len(msgs)
		sm.mu.Unlock()
	} else if err := json.Unmarshal([]byte(messagesJSON), &msgs); err != nil {
		log.Printf("[MySQLSessionManager] unmarshal error for %q: %v", sessionKey, err)
		return []Message{{Role: "system", Content: systemPrompt}}, nil
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

	return result, nil
}

// SaveHistory atomically updates the in-memory cache and persists the row to
// MySQL (and to the MessageStore when configured). Triggers background
// summarization when the configured threshold is crossed.
func (sm *MySQLSessionManager) SaveHistory(ctx context.Context, sessionKey string, msgs []Message) error {
	sm.mu.Lock()
	session, ok := sm.sessions[sessionKey]
	if !ok {
		session = &Session{Key: sessionKey}
		sm.sessions[sessionKey] = session
	}
	cp := make([]Message, len(msgs))
	copy(cp, msgs)
	session.Messages = cp
	sm.mu.Unlock()
	return sm.persist(ctx, sessionKey)
}

// AsyncTasks returns a copy of the background tasks parked on sessionKey.
// Reads from the in-memory cache when present, otherwise lazy-loads from
// MySQL.
func (sm *MySQLSessionManager) AsyncTasks(ctx context.Context, sessionKey string) (map[string]AsyncTask, error) {
	sm.mu.RLock()
	session, ok := sm.sessions[sessionKey]
	sm.mu.RUnlock()

	if ok && session.AsyncTasks != nil {
		cp := make(map[string]AsyncTask, len(session.AsyncTasks))
		for k, v := range session.AsyncTasks {
			cp[k] = v
		}
		return cp, nil
	}

	var asyncTasksJSON sql.NullString
	err := sm.db.QueryRowContext(ctx, fmt.Sprintf("SELECT async_tasks FROM %s WHERE session_key = ?", sm.tableName), sessionKey).Scan(&asyncTasksJSON)
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
			return cp, nil
		}
	}
	return map[string]AsyncTask{}, nil
}

// SaveAsyncTasks atomically updates the in-memory cache and persists the row
// to MySQL.
func (sm *MySQLSessionManager) SaveAsyncTasks(ctx context.Context, sessionKey string, tasks map[string]AsyncTask) error {
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
func (sm *MySQLSessionManager) UpdateBehaviorSummary(sessionKey string, newSummary string) error {
	sm.mu.Lock()
	sm.behaviors[sessionKey] = newSummary
	sm.mu.Unlock()
	// Will be persisted on next Save()
	return nil
}

// Fork creates a new session whose message history is a copy of the first
// atIndex messages from sessionKey. The copy is persisted by inserting a new
// row in agent_sessions. See SessionManager.Fork for full semantics.
func (sm *MySQLSessionManager) Fork(ctx context.Context, sessionKey string, atIndex int) (string, error) {
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

// Delete removes the session from the in-memory cache and deletes the
// corresponding row from the agent_sessions table. Deleting a non-existent
// session is a no-op. When a MessageStore is plugged in, its rows are
// dropped too so no orphan messages survive.
func (sm *MySQLSessionManager) Delete(ctx context.Context, sessionKey string) error {
	sm.mu.Lock()
	delete(sm.sessions, sessionKey)
	delete(sm.behaviors, sessionKey)
	delete(sm.lastSumLen, sessionKey)
	delete(sm.persistedCount, sessionKey)
	sm.mu.Unlock()

	if sm.MessageStore != nil {
		if err := sm.MessageStore.Delete(ctx, sessionKey); err != nil {
			return fmt.Errorf("history: delete session %q via MessageStore: %w", sessionKey, err)
		}
	}
	if _, err := sm.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE session_key = ?", sm.tableName), sessionKey); err != nil {
		return fmt.Errorf("history: delete session %q: %w", sessionKey, err)
	}
	return nil
}

// persist writes the in-memory session row to MySQL and triggers background
// summarization when configured. Internal helper — the SessionManager
// interface no longer exposes a standalone Save method.
func (sm *MySQLSessionManager) persist(ctx context.Context, sessionKey string) error {
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

	asyncTasksBytes, _ := json.Marshal(session.AsyncTasks)

	// When a MessageStore is plugged in, messages live outside the JSON
	// column — write a placeholder and delegate. The persistedCount hint
	// lets the store skip rewriting messages already on disk.
	if sm.MessageStore != nil {
		sm.mu.RLock()
		known := sm.persistedCount[sessionKey]
		sm.mu.RUnlock()
		newCount, err := sm.MessageStore.Save(ctx, sessionKey, cp, known)
		if err != nil {
			return fmt.Errorf("history: MessageStore.Save: %w", err)
		}
		sm.mu.Lock()
		sm.persistedCount[sessionKey] = newCount
		sm.mu.Unlock()

		query := fmt.Sprintf(`
			INSERT INTO %s (session_key, messages, behavior, async_tasks)
			VALUES (?, '[]', ?, ?)
			ON DUPLICATE KEY UPDATE behavior = VALUES(behavior), async_tasks = VALUES(async_tasks)
		`, sm.tableName)
		_, err = sm.db.ExecContext(ctx, query, sessionKey, behavior, string(asyncTasksBytes))
		return err
	}

	msgsBytes, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	query := fmt.Sprintf(`
		INSERT INTO %s (session_key, messages, behavior, async_tasks)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE messages = VALUES(messages), behavior = VALUES(behavior), async_tasks = VALUES(async_tasks)
	`, sm.tableName)
	_, err = sm.db.ExecContext(ctx, query, sessionKey, string(msgsBytes), behavior, string(asyncTasksBytes))
	return err
}

// Query lists sessions whose key starts with prefix. See SessionManager.Query
// for full semantics. Backed by `WHERE session_key LIKE ?` with the prefix
// LIKE-escaped — `_` and `%` in the prefix are treated literally.
func (sm *MySQLSessionManager) Query(ctx context.Context, prefix string, opts SessionQueryOpts) ([]SessionMeta, error) {
	conds := []string{"session_key LIKE ?"}
	args := []any{escapeLikePrefix(prefix) + "%"}
	if !opts.IncludeDeleted {
		conds = append(conds, "deleted_at IS NULL")
	}
	order := "updated_at DESC"
	switch opts.OrderBy {
	case SessionMetaOrderOldest:
		order = "updated_at ASC"
	case SessionMetaOrderKey:
		order = "session_key ASC"
	}
	q := fmt.Sprintf(`
		SELECT session_key, updated_at, deleted_at, JSON_LENGTH(messages)
		FROM %s
		WHERE %s
		ORDER BY %s
	`, sm.tableName, strings.Join(conds, " AND "), order)
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", opts.Limit)
		if opts.Offset > 0 {
			q += fmt.Sprintf(" OFFSET %d", opts.Offset)
		}
	} else if opts.Offset > 0 {
		// MySQL requires LIMIT before OFFSET; use a large sentinel.
		q += fmt.Sprintf(" LIMIT 18446744073709551615 OFFSET %d", opts.Offset)
	}

	rows, err := sm.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("history: query: %w", err)
	}
	defer rows.Close()

	out := make([]SessionMeta, 0, 16)
	for rows.Next() {
		var key string
		var updatedAt time.Time
		var deletedAt sql.NullTime
		var msgCount sql.NullInt64
		if err := rows.Scan(&key, &updatedAt, &deletedAt, &msgCount); err != nil {
			return nil, fmt.Errorf("history: query scan: %w", err)
		}
		meta := SessionMeta{Key: key, UpdatedAt: updatedAt, MessageCount: int(msgCount.Int64)}
		if deletedAt.Valid {
			d := deletedAt.Time
			meta.DeletedAt = &d
		}
		out = append(out, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: query iter: %w", err)
	}
	return out, nil
}

// SoftDelete sets deleted_at = NOW() on the row and evicts the cache.
// Reads (GetHistory / Query without IncludeDeleted) treat the session as
// missing while the row stays behind for Restore.
func (sm *MySQLSessionManager) SoftDelete(ctx context.Context, sessionKey string) error {
	sm.mu.Lock()
	delete(sm.sessions, sessionKey)
	delete(sm.behaviors, sessionKey)
	delete(sm.lastSumLen, sessionKey)
	sm.mu.Unlock()

	q := fmt.Sprintf("UPDATE %s SET deleted_at = NOW() WHERE session_key = ? AND deleted_at IS NULL", sm.tableName)
	if _, err := sm.db.ExecContext(ctx, q, sessionKey); err != nil {
		return fmt.Errorf("history: soft delete %q: %w", sessionKey, err)
	}
	return nil
}

// Restore clears deleted_at on the row.
func (sm *MySQLSessionManager) Restore(ctx context.Context, sessionKey string) error {
	q := fmt.Sprintf("UPDATE %s SET deleted_at = NULL WHERE session_key = ?", sm.tableName)
	if _, err := sm.db.ExecContext(ctx, q, sessionKey); err != nil {
		return fmt.Errorf("history: restore %q: %w", sessionKey, err)
	}
	return nil
}

// PurgeDeletedBefore hard-deletes every soft-deleted row whose deleted_at
// is strictly older than `before`. Returns the count purged.
func (sm *MySQLSessionManager) PurgeDeletedBefore(ctx context.Context, before time.Time) (int, error) {
	q := fmt.Sprintf("DELETE FROM %s WHERE deleted_at IS NOT NULL AND deleted_at < ?", sm.tableName)
	res, err := sm.db.ExecContext(ctx, q, before)
	if err != nil {
		return 0, fmt.Errorf("history: purge: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// escapeLikePrefix protects the user-supplied prefix from LIKE-pattern
// metacharacters so callers that pass arbitrary strings (e.g. raw user IDs)
// don't get unexpected wildcard matches.
func escapeLikePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(prefix)
}

// MySQL error-code helpers. We probe by string match because the
// go-sql-driver/mysql error type isn't part of database/sql and importing
// it just for these checks would invert the dep direction.
func isMySQLDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1060")
}

func isMySQLDuplicateKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1061")
}
