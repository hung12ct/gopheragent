package history

import "context"

// MessageStore is an optional persistence layer for session messages,
// decoupled from session metadata. The default MySQLSessionManager keeps
// messages inside the JSON column on the same row as behavior/async tasks
// (whole-blob storage, simple but writes the entire history on every
// turn). Pass WithMySQLMessageStore to flip to a per-row store —
// MySQLAppendOnlyMessageStore is the canonical implementation; integrators
// can plug in their own (DynamoDB, Redis Streams, S3, etc.).
//
// Hot-path note: implementations should support the knownPersistedCount
// hint so per-turn cost is O(new messages) rather than O(total session).
type MessageStore interface {
	// Save persists messages for sessionKey. knownPersistedCount is the
	// count returned by the previous Save (or by Load on a fresh load) —
	// implementations use it as a hint to skip rewriting messages already
	// on disk. If the slice is shorter than knownPersistedCount the store
	// must treat the change as a truncation and remove the orphan tail.
	// Returns the new persisted count (typically len(messages)).
	Save(ctx context.Context, sessionKey string, messages []Message, knownPersistedCount int) (int, error)
	// Load returns every message persisted for sessionKey, in order.
	Load(ctx context.Context, sessionKey string) ([]Message, error)
	// Delete removes every message stored for sessionKey. No-op if none exist.
	Delete(ctx context.Context, sessionKey string) error
}
