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

func TestGenerateStream_ThoughtSignatureSurfacedAsProviderMeta(t *testing.T) {
	// "c2lnLTE=" is base64 for "sig-1": the wire carries signatures encoded.
	p := streamProvider(t,
		`{"candidates":[{"content":{"role":"model","parts":[`+
			`{"functionCall":{"name":"search","args":{"q":"a"}},"thoughtSignature":"c2lnLTE="},`+
			`{"functionCall":{"name":"fetch","args":{}}}]}}]}`,
		`{"candidates":[{"finishReason":"STOP"}]}`,
	)
	res, _, err := runStream(t, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(res.ToolCalls))
	}
	if got := string(res.ToolCalls[0].ProviderMeta); got != "sig-1" {
		t.Fatalf("signed call ProviderMeta = %q, want sig-1", got)
	}
	if res.ToolCalls[1].ProviderMeta != nil {
		t.Fatalf("unsigned call must carry no ProviderMeta, got %q", res.ToolCalls[1].ProviderMeta)
	}
}

// Gemini rejects a replayed function call whose turn lost its signature, so
// a stored signature must go back verbatim, and a first call that never had
// one must carry the placeholder that switches validation off.
func TestGenerateStream_ReplaysThoughtSignatures(t *testing.T) {
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
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []history.ToolCall{
			{ID: "search", Name: "search", ProviderMeta: []byte("sig-1")},
			{ID: "fetch", Name: "fetch"},
		}},
		{Role: "tool", ToolCallID: "search", Content: "A"},
		{Role: "tool", ToolCallID: "fetch", Content: "B"},
		{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "legacy", Name: "legacy"}}},
		{Role: "tool", ToolCallID: "legacy", Content: "C"},
	}
	ch := make(chan agent.StreamEvent, 16)
	if _, err := p.GenerateStream(context.Background(), memory, nil, ch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req struct {
		Contents []struct {
			Parts []struct {
				FunctionCall     *struct{ Name string } `json:"functionCall"`
				ThoughtSignature []byte                 `json:"thoughtSignature"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	got := map[string]string{}
	for _, c := range req.Contents {
		for _, part := range c.Parts {
			if part.FunctionCall != nil {
				got[part.FunctionCall.Name] = string(part.ThoughtSignature)
			}
		}
	}
	want := map[string]string{"search": "sig-1", "fetch": "", "legacy": string(skipThoughtSignature)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("replayed signatures = %v, want %v", got, want)
	}
}
