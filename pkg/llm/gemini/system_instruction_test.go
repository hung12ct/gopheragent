package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"google.golang.org/genai"
)

// A loop hint appended as a trailing system message used to replace the
// system prompt outright, because only the last system message was kept.
func TestGenerateStream_AllSystemMessagesReachSystemInstruction(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\r\n\r\n")
	}))
	t.Cleanup(srv.Close)
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:      "test-key",
		Backend:     genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL},
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	p := &Provider{client: client, model: "gemini-test"}

	memory := []history.Message{
		{Role: "system", Content: "you are a travel agent"},
		{Role: "user", Content: "hi"},
		{Role: "system", Content: "wrap up now"},
	}
	if _, err := p.GenerateStream(context.Background(), memory, nil, make(chan agent.StreamEvent, 16)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req struct {
		SystemInstruction struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		Contents []json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	var got []string
	for _, part := range req.SystemInstruction.Parts {
		got = append(got, part.Text)
	}
	if want := []string{"you are a travel agent", "wrap up now"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("systemInstruction parts = %q, want %q", got, want)
	}
	if len(req.Contents) != 1 {
		t.Fatalf("contents = %d, want only the user message", len(req.Contents))
	}
}
