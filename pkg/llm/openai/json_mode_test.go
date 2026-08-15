package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
)

func personSchema() agent.StructuredOutput {
	return agent.StructuredOutput{
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
}

// Endpoints that publish only the older json_object format 400 on
// json_schema, so the schema has to move from response_format into the
// prompt while response_format drops to the shape they accept.
func TestJSONModeObject_SendsJSONObjectAndPromptsSchema(t *testing.T) {
	req := captureOpenAIRequest(t, func(p *Provider) {
		p.jsonMode = JSONModeObject
		ctx := agent.WithStructuredOutput(context.Background(), personSchema())
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
	if rf["type"] != "json_object" {
		t.Fatalf("type: want json_object, got %v", rf["type"])
	}
	if _, present := rf["json_schema"]; present {
		t.Fatalf("json_schema must not be sent in object mode: %v", rf)
	}

	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("want 2 messages (user + appended schema), got %v", req["messages"])
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "system" {
		t.Fatalf("appended message role = %v, want system", last["role"])
	}
	content, _ := last["content"].(string)
	if !strings.Contains(content, `"name"`) || !strings.Contains(content, "object") {
		t.Fatalf("appended message must carry the schema, got %q", content)
	}
	// Endpoints in this mode commonly reject a request whose messages never
	// say JSON, so the rendered instruction has to contain the literal word.
	if !strings.Contains(content, "JSON") {
		t.Fatalf("appended message must contain the word JSON, got %q", content)
	}
	if !strings.Contains(content, "a human") {
		t.Fatalf("appended message should carry the schema description, got %q", content)
	}
}

// The prompt-injected schema is a per-request concern; leaking it into the
// conversation the caller passed would repeat it on every later turn.
func TestJSONModeObject_DoesNotMutateCallerMessages(t *testing.T) {
	memory := []history.Message{{Role: "user", Content: "hi"}}
	captureOpenAIRequest(t, func(p *Provider) {
		p.jsonMode = JSONModeObject
		ctx := agent.WithStructuredOutput(context.Background(), personSchema())
		ch := make(chan agent.StreamEvent, 4)
		go func() {
			for range ch {
			}
		}()
		_, _ = p.GenerateStream(ctx, memory, nil, ch)
		close(ch)
	})
	if len(memory) != 1 {
		t.Fatalf("caller history mutated: len = %d, want 1", len(memory))
	}
}

// Object mode still must not send response_format on an ordinary call.
func TestJSONModeObject_NoStructuredOutputOmitsResponseFormat(t *testing.T) {
	req := captureOpenAIRequest(t, func(p *Provider) {
		p.jsonMode = JSONModeObject
		ch := make(chan agent.StreamEvent, 4)
		go func() {
			for range ch {
			}
		}()
		_, _ = p.GenerateStream(context.Background(), []history.Message{{Role: "user", Content: "hi"}}, nil, ch)
		close(ch)
	})
	if _, ok := req["response_format"]; ok {
		t.Fatalf("response_format must be omitted without a schema, got %v", req["response_format"])
	}
}

// Failing loudly beats downgrading to prose the caller will try to
// unmarshal, and beats a 400 whose message names neither the cause nor the
// fix. The request must not reach the endpoint at all.
func TestJSONModeNone_FailsStructuredOutputBeforeSending(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	p, err := NewCompat("test-key", "test/model", srv.URL+"/v1")
	if err != nil {
		t.Fatalf("NewCompat: %v", err)
	}
	ctx := agent.WithStructuredOutput(context.Background(), personSchema())
	ch := make(chan agent.StreamEvent, 4)
	go func() {
		for range ch {
		}
	}()
	_, err = p.GenerateStream(ctx, []history.Message{{Role: "user", Content: "hi"}}, nil, ch)
	close(ch)

	if reached {
		t.Fatal("request must not be sent when no JSON mode is declared")
	}
	if err == nil {
		t.Fatal("want an error when structured output is requested with JSONModeNone")
	}
	if !strings.Contains(err.Error(), "WithJSONMode") {
		t.Fatalf("error must name the fix, got %v", err)
	}
}

// Without a schema on the context, a JSONModeNone provider is an ordinary
// chat provider and must keep working.
func TestJSONModeNone_AllowsPlainChat(t *testing.T) {
	req := captureOpenAIRequest(t, func(p *Provider) {
		p.jsonMode = JSONModeNone
		ch := make(chan agent.StreamEvent, 4)
		go func() {
			for range ch {
			}
		}()
		if _, err := p.GenerateStream(context.Background(), []history.Message{{Role: "user", Content: "hi"}}, nil, ch); err != nil {
			t.Errorf("plain chat must not fail under JSONModeNone: %v", err)
		}
		close(ch)
	})
	if _, ok := req["response_format"]; ok {
		t.Fatalf("response_format must be omitted, got %v", req["response_format"])
	}
}

func TestJSONModeString(t *testing.T) {
	for mode, want := range map[JSONMode]string{
		JSONModeSchema: "JSONModeSchema",
		JSONModeObject: "JSONModeObject",
		JSONModeNone:   "JSONModeNone",
		JSONMode(9):    "JSONMode(9)",
	} {
		if got := mode.String(); got != want {
			t.Fatalf("String() = %q, want %q", got, want)
		}
	}
}
