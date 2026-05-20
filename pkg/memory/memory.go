// Package memory provides cross-session note persistence so agents can
// learn between sessions without retraining.
//
// The package owns three pieces:
//
//   - Note: one distilled fact, preference, or correction.
//   - Store: persistence contract scoped by an opaque ScopeKey (typically a
//     user id, tenant id, or sessionKey when no higher scope exists).
//   - FormatNotes: rendering helper that produces the text block prepended
//     to a fresh session's system message.
//
// Consolidation (turning a closed session's transcript into Notes) lives in
// pkg/agent so it can reuse the loop's LLMProvider; this package stays a
// pure data layer with no LLM dependency.
//
// Concurrency: every Store implementation must be safe for concurrent
// Put/List/Delete calls. Reads dominate writes (the loader pulls notes on
// every fresh session start), so backends should favor read paths.
package memory

import (
	"context"
	"strings"
	"time"
)

// Note is a single piece of distilled knowledge persisted across sessions.
//
// Keep Content short — a one-line fact, preference, or correction. Notes
// are concatenated into the system prompt on every fresh session, so token
// cost scales linearly with the per-scope note count. The consolidator
// caps emission at MaxNotes; adopters that bypass it should self-impose a
// cap of their own.
type Note struct {
	// Key is the stable identifier within (scope, key). Re-Putting an
	// existing key updates the note in place; pick keys that describe the
	// fact (e.g. "jira.default_workspace") rather than session-derived
	// IDs so the consolidator can de-duplicate across runs.
	Key string `json:"key"`
	// Content is the note body. Plain text, kept short.
	Content string `json:"content"`
	// Tags is an optional taxonomy (e.g. "preference", "fact", "mistake").
	// Adopters may filter by tag at format time; not persisted by every
	// backend in indexed form.
	Tags []string `json:"tags,omitempty"`
	// CreatedAt is stamped on first Put and preserved across updates.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is stamped on every Put. Stores return List ordered by
	// UpdatedAt descending so the most recent knowledge appears first.
	UpdatedAt time.Time `json:"updated_at"`
}

// Store is the persistence contract for cross-session memory.
//
// Implementations must be safe for concurrent use. Backends fall into the
// same three buckets as pkg/history: InMem (process-local, fast, lost on
// restart), File (single-node persistent), and remote (DB-backed). Today
// only InMem ships in-tree; the others can be added without changing this
// interface.
type Store interface {
	// Put inserts or updates a note for (scope, note.Key). The
	// implementation stamps UpdatedAt; CreatedAt is preserved on update.
	// Empty Key returns an error.
	Put(ctx context.Context, scope string, note Note) error
	// List returns every note under scope, ordered by UpdatedAt
	// descending. An unseen scope returns (nil, nil).
	List(ctx context.Context, scope string) ([]Note, error)
	// Delete removes one note. No-op when missing.
	Delete(ctx context.Context, scope, key string) error
}

// FormatNotes renders notes for system-prompt injection. The output is
// stable: same notes produce same text, so prompt-cache prefixes stay
// warm across calls. Returns "" when notes is empty so callers can
// concatenate unconditionally without checking length.
//
// The format is intentionally plain text (not JSON) so the LLM treats it
// as natural-language context. Adopters can swap this for a custom
// renderer by formatting notes themselves before injecting.
func FormatNotes(notes []Note) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Long-term memory\n")
	b.WriteString("Facts learned from prior sessions with this user. Use them to skip clarifying questions and avoid repeating past mistakes.\n")
	for _, n := range notes {
		if n.Content == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(n.Content)
		b.WriteString("\n")
	}
	return b.String()
}
