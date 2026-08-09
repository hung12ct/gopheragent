package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// requirePath fails clearly when the server was never reached, instead of
// panicking on a nil URL.
func requirePath(t *testing.T, seen *http.Request, suffix string) {
	t.Helper()
	if seen.URL == nil {
		t.Fatalf("no request reached the local server; the client never left api.openai.com")
	}
	if !strings.HasSuffix(seen.URL.Path, suffix) {
		t.Fatalf("request path = %q, want a local path ending in %q", seen.URL.Path, suffix)
	}
}

// recordingServer captures the path and headers of the first request it sees.
func recordingServer(t *testing.T, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	var seen http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r
		seen.Header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// The regression this whole change exists for: without WithBaseURL these
// clients call api.openai.com, so a deliberately local deployment leaks text
// to a third party and demands a key the operator may not hold.
func TestNonChatClientsHonourBaseURL(t *testing.T) {
	t.Run("embedder", func(t *testing.T) {
		srv, seen := recordingServer(t, `{"data":[{"embedding":[0.1,0.2],"index":0}]}`)
		e, err := NewEmbedder("k", "nomic-embed-text", WithBaseURL(srv.URL+"/v1"))
		if err != nil {
			t.Fatalf("NewEmbedder: %v", err)
		}
		if _, err := e.Embed(context.Background(), []string{"hello"}); err != nil {
			t.Fatalf("Embed: %v", err)
		}
		requirePath(t, seen, "/embeddings")
	})

	t.Run("vision analyzer", func(t *testing.T) {
		srv, seen := recordingServer(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		v, err := NewVisionAnalyzer("k", "llava", WithBaseURL(srv.URL+"/v1"))
		if err != nil {
			t.Fatalf("NewVisionAnalyzer: %v", err)
		}
		if _, err := v.Analyze(context.Background(), "https://example.com/a.png", "describe"); err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		requirePath(t, seen, "/chat/completions")
	})

	t.Run("summary provider", func(t *testing.T) {
		srv, seen := recordingServer(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		sp, err := NewSummaryProvider("k", "llama3", WithBaseURL(srv.URL+"/v1"))
		if err != nil {
			t.Fatalf("NewSummaryProvider: %v", err)
		}
		// Non-empty messages required: SummarizeBehaviors short-circuits on
		// an empty slice and never reaches the transport.
		msgs := []history.Message{{Role: "user", Content: "hello"}}
		if _, err := sp.SummarizeBehaviors(context.Background(), msgs, ""); err != nil {
			t.Fatalf("SummarizeBehaviors: %v", err)
		}
		requirePath(t, seen, "/chat/completions")
	})
}

// Transport options apply to the non-chat clients too, not just the provider.
func TestNonChatClientsHonourHTTPHeader(t *testing.T) {
	srv, seen := recordingServer(t, `{"data":[{"embedding":[0.1],"index":0}]}`)
	e, err := NewEmbedder("k", "m", WithBaseURL(srv.URL+"/v1"), WithHTTPHeader("X-Title", "gopheragent"))
	if err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}
	if _, err := e.Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := seen.Header.Get("X-Title"); got != "gopheragent" {
		t.Fatalf("X-Title = %q, want gopheragent", got)
	}
}

// Every constructor validates through the same helper, so a bad endpoint
// fails at construction rather than at the first request.
func TestBaseURLValidationAppliesToEveryConstructor(t *testing.T) {
	for _, bad := range []string{
		"openrouter.ai/api/v1",
		"https://user:secret@openrouter.ai/api/v1",
		"https://openrouter.ai/api/v1?key=value",
		"https://openrouter.ai/api/v1#fragment",
	} {
		t.Run(bad, func(t *testing.T) {
			if _, err := New("k", "m", WithBaseURL(bad)); err == nil {
				t.Fatal("New accepted a malformed baseURL")
			}
			if _, err := NewEmbedder("k", "m", WithBaseURL(bad)); err == nil {
				t.Fatal("NewEmbedder accepted a malformed baseURL")
			}
			if _, err := NewVisionAnalyzer("k", "m", WithBaseURL(bad)); err == nil {
				t.Fatal("NewVisionAnalyzer accepted a malformed baseURL")
			}
			if _, err := NewSummaryProvider("k", "m", WithBaseURL(bad)); err == nil {
				t.Fatal("NewSummaryProvider accepted a malformed baseURL")
			}
		})
	}
}

// A trailing slash is normalized away because the SDK appends its own path
// segment; leaving it produces a double slash the gateway may reject.
func TestValidateBaseURLTrimsTrailingSlash(t *testing.T) {
	got, err := validateBaseURL("  https://openrouter.ai/api/v1/  ")
	if err != nil {
		t.Fatalf("validateBaseURL: %v", err)
	}
	if got != "https://openrouter.ai/api/v1" {
		t.Fatalf("validateBaseURL = %q, want the trimmed form", got)
	}
}
