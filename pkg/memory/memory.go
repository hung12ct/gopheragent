// Package memory provides cross-session note persistence so agents can
// learn between sessions without retraining.
//
// Three pieces:
//
//   - Note: one distilled fact, preference, or correction.
//   - Store: persistence contract scoped by an opaque ScopeKey (typically a
//     user id, tenant id, or sessionKey when no higher scope exists).
//   - FormatNotes: rendering helper that produces the text block prepended
//     to a session's system prompt by the agent loader.
//
// Bounded growth is a hard requirement of every Store implementation:
// either via a per-scope cap enforced inside Put (the InMemStore path)
// or via a periodic Compactor that calls ReplaceAll with a curated
// subset (the agent.Consolidator path). Without one of these in place,
// uncurated note streams would grow the loader's prompt linearly and
// eventually exhaust the model context.
//
// Consolidation (turning a closed session's transcript into Notes) lives
// in pkg/agent so it can reuse the loop's LLMProvider; this package
// stays a pure data layer with no LLM dependency.
//
// Concurrency: every Store implementation must be safe for concurrent
// Put/List/Delete/ReplaceAll. Reads dominate writes — the loader pulls
// notes on every Run start — so backends should favor read paths.
package memory

import (
	"context"
	"strings"
	"time"
)

// Note is a single piece of distilled knowledge persisted across sessions.
//
// Keep Content short — a one-line fact, preference, or correction. The
// agent loader concatenates notes into the system prompt, so token cost
// scales linearly with the per-scope note count up to the loader's
// budget cap. Store implementations enforce a hard per-scope cap on top
// of the loader cap to prevent runaway growth from undisciplined
// callers.
type Note struct {
	// Key is the stable identifier within (scope, key). Re-Putting an
	// existing key updates the note in place; pick keys that describe the
	// fact (e.g. "jira.default_workspace") rather than session-derived
	// IDs (e.g. "fact_2026_05_20") so updates collapse instead of
	// piling new rows on top of stale ones.
	Key string `json:"key"`
	// Content is the note body. Plain text, kept short.
	Content string `json:"content"`
	// Tags is an optional taxonomy (e.g. "preference", "fact", "mistake").
	// ListOpts.Tags filters on this; the loader treats tags as
	// observational only.
	Tags []string `json:"tags,omitempty"`
	// CreatedAt is stamped on first Put and preserved across updates.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is stamped on every Put. Stores return List ordered by
	// UpdatedAt descending so the most recent knowledge appears first
	// and per-scope eviction targets the oldest entries.
	UpdatedAt time.Time `json:"updated_at"`
}

// ListOpts narrows a Store.List read. Zero values disable each filter,
// so the empty struct is equivalent to "everything for this scope".
//
// Limit + UpdatedAfter together let the loader pull only the most
// recent N notes — bounded both by count and by recency so a long-dead
// scope can't dominate a fresh one.
type ListOpts struct {
	// Limit caps the number of notes returned. 0 means unlimited.
	// Stores apply the limit after sorting (newest first), so callers
	// always get the most recent N. Negative values are clamped to 0.
	Limit int
	// Tags, when non-empty, returns only notes that carry every listed
	// tag (AND filter). Empty (the default) returns all notes
	// regardless of tags.
	Tags []string
	// UpdatedAfter, when non-zero, returns only notes whose UpdatedAt
	// is strictly after this cutoff. Pair with a "last seen 30 days
	// ago" cutoff to prune stale knowledge from the loader prompt
	// without deleting it from storage.
	UpdatedAfter time.Time
}

// Store is the persistence contract for cross-session memory.
//
// Implementations must be safe for concurrent use. Backends fall into the
// same three buckets as pkg/history: InMem (process-local, fast, lost on
// restart), File (single-node persistent), and remote (DB-backed). Today
// only InMem ships in-tree; the others can be added without changing
// this interface.
type Store interface {
	// Put inserts or updates a note for (scope, note.Key). The
	// implementation stamps UpdatedAt; CreatedAt is preserved on
	// update. Empty Key returns an error. Implementations may enforce
	// a per-scope cap by evicting the least-recently-updated note when
	// the cap is exceeded; that eviction is a normal Put outcome, not
	// an error.
	Put(ctx context.Context, scope string, note Note) error
	// List returns notes under scope, ordered by UpdatedAt descending,
	// filtered and bounded by opts. An unseen scope returns (nil, nil).
	List(ctx context.Context, scope string, opts ListOpts) ([]Note, error)
	// Delete removes one note. No-op when missing.
	Delete(ctx context.Context, scope, key string) error
	// ReplaceAll atomically replaces every note under scope with the
	// supplied set. Used by the Consolidator's merge path so a curated
	// new state lands without a window where deletions and inserts are
	// observable separately. CreatedAt timestamps are preserved when an
	// incoming note's Key matches an existing one; new keys get a
	// fresh CreatedAt. UpdatedAt is stamped on every entry. An empty
	// slice clears the scope.
	ReplaceAll(ctx context.Context, scope string, notes []Note) error
}

// FormatNotes renders notes for system-prompt injection. The output is
// stable: same notes produce same text, so prompt-cache prefixes stay
// warm across calls within a Run. Returns "" when notes is empty so
// callers can concatenate unconditionally without checking length.
//
// The format is intentionally plain text (not JSON) so the LLM treats
// it as natural-language context. Adopters can swap this for a custom
// renderer by formatting notes themselves before injection.
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

// matchesTags reports whether n carries every tag in required. A
// nil/empty required filter matches every note. Helper for Store
// implementations applying ListOpts.Tags.
func matchesTags(n Note, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(n.Tags))
	for _, t := range n.Tags {
		have[t] = struct{}{}
	}
	for _, t := range required {
		if _, ok := have[t]; !ok {
			return false
		}
	}
	return true
}
