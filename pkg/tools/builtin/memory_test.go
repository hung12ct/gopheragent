package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
)

func memCtx(sessionKey string) context.Context {
	return agent.WithSessionKey(context.Background(), sessionKey)
}

func TestMemoryTools_SetGetRoundTrip(t *testing.T) {
	store := NewInMemoryStore()
	set := NewMemorySetTool(store)
	get := NewMemoryGetTool(store)
	ctx := memCtx("sess-1")

	if _, err := set.Execute(ctx, `{"key":"name","value":"alice"}`); err != nil {
		t.Fatalf("set: %v", err)
	}
	out, err := get.Execute(ctx, `{"key":"name"}`)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var env struct {
		Found bool   `json:"found"`
		Value string `json:"value"`
	}
	_ = json.Unmarshal([]byte(out.Text), &env)
	if !env.Found || env.Value != "alice" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
}

func TestMemoryTools_SessionsAreIsolated(t *testing.T) {
	store := NewInMemoryStore()
	set := NewMemorySetTool(store)
	get := NewMemoryGetTool(store)

	if _, err := set.Execute(memCtx("s1"), `{"key":"k","value":"one"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := set.Execute(memCtx("s2"), `{"key":"k","value":"two"}`); err != nil {
		t.Fatal(err)
	}
	out, _ := get.Execute(memCtx("s1"), `{"key":"k"}`)
	if !strings.Contains(out.Text, `"value":"one"`) {
		t.Fatalf("s1 cross-talk: %s", out.Text)
	}
	out, _ = get.Execute(memCtx("s2"), `{"key":"k"}`)
	if !strings.Contains(out.Text, `"value":"two"`) {
		t.Fatalf("s2 cross-talk: %s", out.Text)
	}
}

func TestMemoryTools_GetMissingKey(t *testing.T) {
	store := NewInMemoryStore()
	get := NewMemoryGetTool(store)
	out, err := get.Execute(memCtx("s"), `{"key":"missing"}`)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Found bool   `json:"found"`
		Value string `json:"value"`
	}
	_ = json.Unmarshal([]byte(out.Text), &env)
	if env.Found || env.Value != "" {
		t.Fatalf("missing key should not be found: %+v", env)
	}
}

func TestMemoryTools_Delete(t *testing.T) {
	store := NewInMemoryStore()
	set := NewMemorySetTool(store)
	del := NewMemoryDeleteTool(store)
	get := NewMemoryGetTool(store)
	ctx := memCtx("s")

	_, _ = set.Execute(ctx, `{"key":"k","value":"v"}`)
	if _, err := del.Execute(ctx, `{"key":"k"}`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	out, _ := get.Execute(ctx, `{"key":"k"}`)
	if !strings.Contains(out.Text, `"found":false`) {
		t.Fatalf("key still present: %s", out.Text)
	}
}

func TestMemoryTools_ListReturnsSortedKeys(t *testing.T) {
	store := NewInMemoryStore()
	set := NewMemorySetTool(store)
	list := NewMemoryListTool(store)
	ctx := memCtx("s")

	for _, k := range []string{"b", "a", "c"} {
		_, _ = set.Execute(ctx, `{"key":"`+k+`","value":"v"}`)
	}
	out, err := list.Execute(ctx, ``)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Keys  []string `json:"keys"`
		Count int      `json:"count"`
	}
	_ = json.Unmarshal([]byte(out.Text), &env)
	if env.Count != 3 {
		t.Fatalf("count: %d", env.Count)
	}
	if env.Keys[0] != "a" || env.Keys[1] != "b" || env.Keys[2] != "c" {
		t.Fatalf("unsorted: %+v", env.Keys)
	}
}

func TestMemoryTools_RejectWhenSessionKeyMissing(t *testing.T) {
	store := NewInMemoryStore()
	set := NewMemorySetTool(store)
	_, err := set.Execute(context.Background(), `{"key":"k","value":"v"}`)
	if err == nil || !strings.Contains(err.Error(), "sessionKey") {
		t.Fatalf("expected sessionKey error, got %v", err)
	}
}

func TestMemoryTools_SetRequiresKey(t *testing.T) {
	set := NewMemorySetTool(NewInMemoryStore())
	_, err := set.Execute(memCtx("s"), `{"key":"","value":"v"}`)
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}
