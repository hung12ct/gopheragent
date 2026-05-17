package history

import (
	"context"
	"testing"
	"time"
)

func seedQuerySessions(t *testing.T, sm *InMemSessionManager, keys ...string) {
	t.Helper()
	for _, k := range keys {
		sm.SaveHistory(context.Background(), k, []Message{
			{Role: "system", Content: "you are…"},
			{Role: "user", Content: "msg from " + k},
		})
		// Force ordering — without this, all sessions land at near-identical
		// timestamps and SessionMetaOrderRecent is non-deterministic.
		time.Sleep(2 * time.Millisecond)
	}
}

func TestInMem_Query_PrefixFiltersAndOrders(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	seedQuerySessions(t, sm, "alice:1", "alice:2", "bob:1")

	got, err := sm.Query(context.Background(), "alice:", SessionQueryOpts{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("prefix filter: got %d sessions, want 2", len(got))
	}
	// Default order is recent-first. alice:2 was seeded last.
	if got[0].Key != "alice:2" {
		t.Fatalf("default order: got %q first, want alice:2", got[0].Key)
	}
	for _, m := range got {
		if m.MessageCount == 0 {
			t.Fatalf("MessageCount should be populated, got %+v", m)
		}
		if m.UpdatedAt.IsZero() {
			t.Fatalf("UpdatedAt should be populated, got %+v", m)
		}
	}
}

func TestInMem_Query_LimitAndOffset(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	seedQuerySessions(t, sm, "s1", "s2", "s3", "s4")

	got, err := sm.Query(context.Background(), "", SessionQueryOpts{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit+offset: got %d, want 2", len(got))
	}
}

func TestInMem_SoftDelete_HiddenByDefaultRevealedByFlag(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	seedQuerySessions(t, sm, "s1", "s2")

	if err := sm.SoftDelete(context.Background(), "s1"); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	visible, _ := sm.Query(context.Background(), "", SessionQueryOpts{})
	if len(visible) != 1 || visible[0].Key != "s2" {
		t.Fatalf("default query should hide soft-deleted s1, got %+v", visible)
	}

	all, _ := sm.Query(context.Background(), "", SessionQueryOpts{IncludeDeleted: true})
	if len(all) != 2 {
		t.Fatalf("IncludeDeleted should reveal s1, got %d sessions", len(all))
	}
	var sawTombstone bool
	for _, m := range all {
		if m.Key == "s1" && m.DeletedAt != nil {
			sawTombstone = true
		}
	}
	if !sawTombstone {
		t.Fatal("DeletedAt not populated on soft-deleted session")
	}
}

func TestInMem_SetTitle_PopulatedInQuery(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	seedQuerySessions(t, sm, "s1", "s2")

	if err := sm.SetTitle(context.Background(), "s1", "Top customers by revenue"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	got, _ := sm.Query(context.Background(), "", SessionQueryOpts{})
	var titled, untitled *SessionMeta
	for i := range got {
		switch got[i].Key {
		case "s1":
			titled = &got[i]
		case "s2":
			untitled = &got[i]
		}
	}
	if titled == nil || titled.Title != "Top customers by revenue" {
		t.Fatalf("titled session Query result missing title, got %+v", titled)
	}
	if untitled == nil || untitled.Title != "" {
		t.Fatalf("untitled session must have empty Title, got %+v", untitled)
	}
}

func TestInMem_SetTitle_EmptyClears(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	seedQuerySessions(t, sm, "s1")
	_ = sm.SetTitle(context.Background(), "s1", "Some title")
	_ = sm.SetTitle(context.Background(), "s1", "")
	got, _ := sm.Query(context.Background(), "", SessionQueryOpts{})
	if len(got) != 1 || got[0].Title != "" {
		t.Fatalf("empty SetTitle should clear, got %+v", got)
	}
}

func TestInMem_SetTitle_BeforeFirstSave(t *testing.T) {
	// Setting a title for a session that hasn't been saved yet is permitted —
	// the typical event-handler path is to call SetTitle concurrently with
	// the first SaveHistory and race outcomes shouldn't matter.
	sm := NewInMemSessionManager("sys")
	if err := sm.SetTitle(context.Background(), "fresh", "Early title"); err != nil {
		t.Fatalf("SetTitle on unseen key: %v", err)
	}
	_ = sm.SaveHistory(context.Background(), "fresh", []Message{{Role: "system", Content: "sys"}})
	got, _ := sm.Query(context.Background(), "", SessionQueryOpts{})
	if len(got) != 1 || got[0].Title != "Early title" {
		t.Fatalf("title set before save not surfaced, got %+v", got)
	}
}

func TestInMem_Restore_ClearsTombstone(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	seedQuerySessions(t, sm, "s1")
	_ = sm.SoftDelete(context.Background(), "s1")
	if err := sm.Restore(context.Background(), "s1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, _ := sm.Query(context.Background(), "", SessionQueryOpts{})
	if len(got) != 1 || got[0].DeletedAt != nil {
		t.Fatalf("restore should clear tombstone, got %+v", got)
	}
}

func TestInMem_PurgeDeletedBefore_HardDeletesOldTombstones(t *testing.T) {
	sm := NewInMemSessionManager("sys")
	seedQuerySessions(t, sm, "old", "fresh")

	// Manually backdate "old"'s tombstone to 1h ago.
	_ = sm.SoftDelete(context.Background(), "old")
	sm.mu.Lock()
	sm.deletedAt["old"] = time.Now().Add(-time.Hour)
	sm.mu.Unlock()
	_ = sm.SoftDelete(context.Background(), "fresh")

	// Purge anything tombstoned > 30 min ago. Only "old" qualifies.
	purged, err := sm.PurgeDeletedBefore(context.Background(), time.Now().Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged != 1 {
		t.Fatalf("expected to purge exactly 1 session, got %d", purged)
	}

	all, _ := sm.Query(context.Background(), "", SessionQueryOpts{IncludeDeleted: true})
	if len(all) != 1 || all[0].Key != "fresh" {
		t.Fatalf("after purge expected only 'fresh' remaining, got %+v", all)
	}
}
