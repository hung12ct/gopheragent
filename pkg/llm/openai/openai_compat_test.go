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
	caps := (&Provider{}).Capabilities()
	if !caps.ImageInput || !caps.StructuredOutput {
		t.Fatalf("Capabilities = %+v, want image input and structured output", caps)
	}
}
