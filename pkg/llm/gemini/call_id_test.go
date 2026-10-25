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

func TestGenerateStream_SameToolCallsGetUniqueIDs(t *testing.T) {
	p := streamProvider(t,
		`{"candidates":[{"content":{"role":"model","parts":[`+
			`{"functionCall":{"name":"search","args":{"q":"a"}}},`+
			`{"functionCall":{"name":"search","args":{"q":"b"}}},`+
			`{"functionCall":{"name":"fetch","args":{}}},`+
			`{"functionCall":{"id":"srv-1","name":"search","args":{"q":"c"}}}]}}]}`,
		`{"candidates":[{"finishReason":"STOP"}]}`,
	)
	res, _, err := runStream(t, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"search", "search#2", "fetch", "srv-1"}
	if len(res.ToolCalls) != len(want) {
		t.Fatalf("got %d tool calls, want %d", len(res.ToolCalls), len(want))
	}
	for i, tc := range res.ToolCalls {
		if tc.ID != want[i] {
			t.Fatalf("call %d ID = %q, want %q", i, tc.ID, want[i])
		}
	}
}

func TestGenerateStream_FunctionResponseNamedByCall(t *testing.T) {
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
			{ID: "search", Name: "search", Arguments: `{"q":"a"}`},
			{ID: "search#2", Name: "search", Arguments: `{"q":"b"}`},
		}},
		{Role: "tool", ToolCallID: "search", Content: "A"},
		{Role: "tool", ToolCallID: "search#2", Content: "B"},
		{Role: "tool", ToolCallID: "legacy", Content: "C"}, // no recorded call
	}
	ch := make(chan agent.StreamEvent, 16)
	if _, err := p.GenerateStream(context.Background(), memory, nil, ch); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req struct {
		Contents []struct {
			Parts []struct {
				FunctionResponse *struct {
					Name string `json:"name"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	var names []string
	for _, c := range req.Contents {
		for _, part := range c.Parts {
			if part.FunctionResponse != nil {
				names = append(names, part.FunctionResponse.Name)
			}
		}
	}
	want := []string{"search", "search", "legacy"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("FunctionResponse names = %v, want %v", names, want)
	}
}
