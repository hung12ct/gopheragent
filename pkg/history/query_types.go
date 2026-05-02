package history

import (
	"sort"
	"time"
)

// SessionMeta is the lightweight per-session shape returned by
// SessionManager.Query. Backends populate every field except DeletedAt,
// which is non-nil only for soft-deleted sessions and only when
// SessionQueryOpts.IncludeDeleted is true (otherwise deleted sessions
// are filtered out at query time).
type SessionMeta struct {
	Key          string     `json:"key"`
	UpdatedAt    time.Time  `json:"updated_at"`
	MessageCount int        `json:"message_count"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
}

// SessionMetaOrder controls the sort key used by SessionManager.Query.
type SessionMetaOrder string

const (
	// SessionMetaOrderRecent sorts by UpdatedAt descending (default — the
	// shape every "list my sessions" sidebar wants out of the box).
	SessionMetaOrderRecent SessionMetaOrder = "updated_at_desc"
	// SessionMetaOrderOldest sorts by UpdatedAt ascending. Useful for
	// audit / archival flows that want chronological order.
	SessionMetaOrderOldest SessionMetaOrder = "updated_at_asc"
	// SessionMetaOrderKey sorts lexicographically by Key. Useful for
	// stable pagination when UpdatedAt may collide at second resolution.
	SessionMetaOrderKey SessionMetaOrder = "key_asc"
)

// SessionQueryOpts configures SessionManager.Query. Zero values are safe
// defaults: Limit=0 means "no limit", OrderBy="" means recent-first,
// IncludeDeleted=false filters soft-deleted sessions out.
type SessionQueryOpts struct {
	Limit          int              // 0 = no limit
	Offset         int              // pagination cursor
	OrderBy        SessionMetaOrder // "" defaults to SessionMetaOrderRecent
	IncludeDeleted bool             // include soft-deleted sessions
}

// sortSessionMeta orders metas in place per the requested SessionMetaOrder.
// Empty / unrecognized order falls back to SessionMetaOrderRecent so callers
// always get a deterministic shape.
func sortSessionMeta(metas []SessionMeta, order SessionMetaOrder) {
	switch order {
	case SessionMetaOrderOldest:
		sort.Slice(metas, func(i, j int) bool { return metas[i].UpdatedAt.Before(metas[j].UpdatedAt) })
	case SessionMetaOrderKey:
		sort.Slice(metas, func(i, j int) bool { return metas[i].Key < metas[j].Key })
	default: // SessionMetaOrderRecent or empty
		sort.Slice(metas, func(i, j int) bool { return metas[i].UpdatedAt.After(metas[j].UpdatedAt) })
	}
}
