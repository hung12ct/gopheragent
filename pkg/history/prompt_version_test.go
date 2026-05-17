package history

import (
	"context"
	"strings"
	"testing"
)

func TestStampPromptVersion_NoOpWhenEmpty(t *testing.T) {
	body := "You are an AI assistant."
	if got := stampPromptVersion("", body); got != body {
		t.Fatalf("empty version must return body unchanged, got %q", got)
	}
}

func TestStampPromptVersion_PrependsMarker(t *testing.T) {
	got := stampPromptVersion("v3", "You are…")
	if !strings.HasPrefix(got, "<!-- prompt-version:v3 -->\n") {
		t.Fatalf("missing version marker, got %q", got)
	}
	if !strings.HasSuffix(got, "You are…") {
		t.Fatalf("body lost after stamping, got %q", got)
	}
}

func TestExtractPromptVersion_RoundTrip(t *testing.T) {
	stamped := stampPromptVersion("v3", "body content")
	version, body := extractPromptVersion(stamped)
	if version != "v3" || body != "body content" {
		t.Fatalf("round trip: got version=%q body=%q", version, body)
	}
}

func TestExtractPromptVersion_NoMarkerLeavesContentUnchanged(t *testing.T) {
	version, body := extractPromptVersion("just a system prompt")
	if version != "" {
		t.Fatalf("unexpected version recovered: %q", version)
	}
	if body != "just a system prompt" {
		t.Fatalf("body altered: %q", body)
	}
}

func TestInMem_PromptVersion_StampedOnGetHistory(t *testing.T) {
	sm := NewInMemSessionManager("you are helpful").WithPromptVersion("v3")
	msgs, _ := sm.History(context.Background(), "fresh")
	if len(msgs) != 1 || msgs[0].Role != "system" {
		t.Fatalf("expected one system message, got %+v", msgs)
	}
	if !strings.HasPrefix(msgs[0].Content, "<!-- prompt-version:v3 -->\n") {
		t.Fatalf("system prompt missing version marker: %q", msgs[0].Content)
	}
}

func TestInMem_PromptVersion_BumpReflectsOnNextRead(t *testing.T) {
	// The user's stated pain point: edit prompt rules, existing sessions
	// keep showing old version. Bump PromptVersion → next read carries v4.
	sm := NewInMemSessionManager("you are helpful").WithPromptVersion("v3")
	sm.SaveHistory(context.Background(), "s1", []Message{{Role: "system", Content: "old"}, {Role: "user", Content: "hi"}})

	got, _ := sm.History(context.Background(), "s1")
	if !strings.HasPrefix(got[0].Content, "<!-- prompt-version:v3 -->\n") {
		t.Fatalf("first read should have v3 marker, got %q", got[0].Content)
	}

	sm.PromptVersion = "v4"
	got, _ = sm.History(context.Background(), "s1")
	if !strings.HasPrefix(got[0].Content, "<!-- prompt-version:v4 -->\n") {
		t.Fatalf("after bump, should have v4 marker, got %q", got[0].Content)
	}
}
