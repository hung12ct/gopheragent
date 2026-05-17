package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/sashabaranov/go-openai"
)

// captureOpenAIRequest spins up an httptest server that records the decoded
// ChatCompletionRequest body and returns an immediate [DONE] SSE frame so
// GenerateStream completes cleanly without needing real streaming content.
func captureOpenAIRequest(t *testing.T, run func(p *OpenAIProvider)) map[string]any {
	t.Helper()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	cfg := openai.DefaultConfig("test-key")
	cfg.BaseURL = srv.URL
	p := &OpenAIProvider{
		client: openai.NewClientWithConfig(cfg),
		model:  "gpt-4o",
	}
	run(p)
	if captured == nil {
		t.Fatal("request body never captured")
	}
	return captured
}

func TestOpenAI_StructuredOutput_SetsResponseFormat(t *testing.T) {
	so := agent.StructuredOutput{
		Name:        "person",
		Description: "a human",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []string{"name"},
		},
		Strict: true,
	}

	req := captureOpenAIRequest(t, func(p *OpenAIProvider) {
		ctx := agent.WithStructuredOutput(context.Background(), so)
		ch := make(chan agent.StreamEvent, 4)
		go func() {
			for range ch {
			}
		}()
		_, _ = p.GenerateStream(ctx, []history.Message{{Role: "user", Content: "hi"}}, nil, ch)
		close(ch)
	})

	rf, ok := req["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing or wrong shape: %v", req["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Fatalf("type: want json_schema, got %v", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("json_schema missing: %v", rf)
	}
	if js["name"] != "person" {
		t.Fatalf("name: want person, got %v", js["name"])
	}
	if js["description"] != "a human" {
		t.Fatalf("description: want 'a human', got %v", js["description"])
	}
	if js["strict"] != true {
		t.Fatalf("strict: want true, got %v", js["strict"])
	}
	schema, ok := js["schema"].(map[string]any)
	if !ok {
		t.Fatalf("schema missing: %v", js)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema.type: want object, got %v", schema["type"])
	}
}

func TestOpenAI_StructuredOutput_DefaultsNameWhenMissing(t *testing.T) {
	// Callers can leave Name empty; the provider should supply a fallback
	// rather than letting OpenAI reject an empty string.
	so := agent.StructuredOutput{
		Schema: map[string]any{"type": "object"},
	}
	req := captureOpenAIRequest(t, func(p *OpenAIProvider) {
		ctx := agent.WithStructuredOutput(context.Background(), so)
		ch := make(chan agent.StreamEvent, 4)
		go func() {
			for range ch {
			}
		}()
		_, _ = p.GenerateStream(ctx, []history.Message{{Role: "user", Content: "hi"}}, nil, ch)
		close(ch)
	})
	rf := req["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if name, _ := js["name"].(string); name == "" {
		t.Fatalf("expected non-empty fallback name, got empty")
	}
}

func TestOpenAI_NoStructuredOutput_OmitsResponseFormat(t *testing.T) {
	// The default path must not send response_format; that would change wire
	// semantics for every non-JSON-mode call.
	req := captureOpenAIRequest(t, func(p *OpenAIProvider) {
		ch := make(chan agent.StreamEvent, 4)
		go func() {
			for range ch {
			}
		}()
		_, _ = p.GenerateStream(context.Background(), []history.Message{{Role: "user", Content: "hi"}}, nil, ch)
		close(ch)
	})
	if _, ok := req["response_format"]; ok {
		t.Fatalf("response_format must be omitted by default, got %v", req["response_format"])
	}
}

// TestOpenAI_AssistantToolCall_HasContentField confirms that an assistant
// message carrying ToolCalls but no text content goes out with a non-empty
// content field. The SDK's omitempty drops empty strings entirely, and
// GPT-5 rejects messages whose content field is missing — see BACKLOG
// "OpenAI provider sends assistant tool-call messages with no content field".
func TestOpenAI_AssistantToolCall_HasContentField(t *testing.T) {
	req := captureOpenAIRequest(t, func(p *OpenAIProvider) {
		ch := make(chan agent.StreamEvent, 4)
		go func() {
			for range ch {
			}
		}()
		_, _ = p.GenerateStream(context.Background(), []history.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "", ToolCalls: []history.ToolCall{{
				ID:        "call_1",
				Name:      "foo",
				Arguments: "{}",
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "ok"},
		}, nil, ch)
		close(ch)
	})

	msgs, ok := req["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing: %v", req["messages"])
	}
	var assistantMsg map[string]any
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "assistant" {
			assistantMsg = mm
			break
		}
	}
	if assistantMsg == nil {
		t.Fatal("assistant message not found in serialized request")
	}
	content, present := assistantMsg["content"]
	if !present {
		t.Fatal("assistant message must serialize a content field (GPT-5 rejects null)")
	}
	if content == nil {
		t.Fatal("assistant message content must not be null")
	}
	if s, _ := content.(string); s == "" {
		t.Fatalf("assistant message content must be non-empty string (got %q)", s)
	}
}

// Every role with empty Content must serialize a non-null content
// field — gpt-4.1 and gpt-5 both 400 with "Invalid value for 'content':
// expected a string, got null." The Go SDK's `omitempty` would drop the
// field entirely for empty strings, so the converter stamps a single
// space across every role. Multimodal messages (MultiContent populated)
// are exempt and remain untouched.
func TestOpenAI_EmptyContent_AllRolesHaveContent(t *testing.T) {
	req := captureOpenAIRequest(t, func(p *OpenAIProvider) {
		ch := make(chan agent.StreamEvent, 8)
		go func() {
			for range ch {
			}
		}()
		_, _ = p.GenerateStream(context.Background(), []history.Message{
			{Role: "system", Content: ""},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "", ToolCalls: []history.ToolCall{{
				ID: "call_1", Name: "foo", Arguments: "{}",
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: ""},
			{Role: "assistant", Content: ""},
		}, nil, ch)
		close(ch)
	})

	msgs, ok := req["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing: %v", req["messages"])
	}
	for i, m := range msgs {
		mm, _ := m.(map[string]any)
		role, _ := mm["role"].(string)
		content, present := mm["content"]
		if !present {
			t.Fatalf("msg[%d] role=%q missing content field (gpt-4.1/gpt-5 will 400)", i, role)
		}
		if content == nil {
			t.Fatalf("msg[%d] role=%q has null content (gpt-4.1/gpt-5 will 400)", i, role)
		}
		if s, _ := content.(string); s == "" {
			t.Fatalf("msg[%d] role=%q has empty-string content; converter must stamp a space", i, role)
		}
	}
}

// Defensive: confirm that the jsonSchemaMarshaler produces a JSON object,
// not the Go map's default encoding quirks, when the map is empty.
func TestJSONSchemaMarshaler_EmptyMap(t *testing.T) {
	raw, err := jsonSchemaMarshaler(map[string]any{}).MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "{}" {
		t.Fatalf("want '{}', got %q", string(raw))
	}
}
