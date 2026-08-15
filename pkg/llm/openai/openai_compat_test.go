package openai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewCompatRejectsMalformedBaseURLs(t *testing.T) {
	for _, baseURL := range []string{
		"openrouter.ai/api/v1",
		"https://user:secret@openrouter.ai/api/v1",
		"https://openrouter.ai/api/v1?key=value",
		"https://openrouter.ai/api/v1#fragment",
	} {
		t.Run(baseURL, func(t *testing.T) {
			_, err := NewCompat("test-key", "test/model", baseURL)
			if err == nil || !strings.Contains(err.Error(), "baseURL") {
				t.Fatalf("NewCompat(%q) error = %v, want baseURL validation", baseURL, err)
			}
		})
	}
}

func TestNewCompatSendsConfiguredHeaders(t *testing.T) {
	var title string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title = r.Header.Get("X-Title")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p, err := NewCompat("test-key", "test/model", srv.URL+"/v1",
		WithHTTPHeader("X-Title", "gopheragent"))
	if err != nil {
		t.Fatalf("NewCompat: %v", err)
	}
	if _, _, err := runStream(t, p); err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if title != "gopheragent" {
		t.Fatalf("X-Title = %q, want gopheragent", title)
	}
}

func TestOpenAIReportsMultimodalStructuredTransport(t *testing.T) {
	p, err := New("test-key", "gpt-4o")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	caps := p.Capabilities()
	if !caps.ImageInput || !caps.StructuredOutput {
		t.Fatalf("Capabilities = %+v, want image input and structured output", caps)
	}
}

// A compatible endpoint implements an unknown subset, so NewCompat must not
// claim OpenAI's features on its behalf: a consumer that checks capabilities
// to reject an unsuitable provider gets a wrong answer at construction and
// discovers the truth as a 400 mid-run.
func TestNewCompatClaimsNothingUntilDeclared(t *testing.T) {
	p, err := NewCompat("test-key", "test/model", "https://api.example.com/v1")
	if err != nil {
		t.Fatalf("NewCompat: %v", err)
	}
	if caps := p.Capabilities(); caps.ImageInput || caps.StructuredOutput {
		t.Fatalf("Capabilities = %+v, want no claims by default", caps)
	}
}

func TestNewCompatCapabilitiesFollowDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
		want bool
	}{
		{"schema", []Option{WithJSONMode(JSONModeSchema)}, true},
		{"object", []Option{WithJSONMode(JSONModeObject)}, true},
		{"none", []Option{WithJSONMode(JSONModeNone)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewCompat("test-key", "test/model", "https://api.example.com/v1", tc.opts...)
			if err != nil {
				t.Fatalf("NewCompat: %v", err)
			}
			if got := p.Capabilities().StructuredOutput; got != tc.want {
				t.Fatalf("StructuredOutput = %v, want %v", got, tc.want)
			}
		})
	}

	// Caller options must beat the defaults NewCompat prepends.
	p, err := NewCompat("test-key", "test/model", "https://api.example.com/v1", WithImageInput(true))
	if err != nil {
		t.Fatalf("NewCompat: %v", err)
	}
	if !p.Capabilities().ImageInput {
		t.Fatal("WithImageInput(true) must override the NewCompat default")
	}
}
