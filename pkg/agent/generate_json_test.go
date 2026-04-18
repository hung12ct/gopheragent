package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// captureProvider is an LLMProvider that records the ctx and messages it
// received, then returns the caller-supplied LLMResult/err.
type captureProvider struct {
	gotCtx      context.Context
	gotMessages []history.Message
	gotTools    *tools.Registry
	result      LLMResult
	err         error
}

func (p *captureProvider) GenerateStream(ctx context.Context, memory []history.Message, avail *tools.Registry, stream chan<- StreamEvent) (LLMResult, error) {
	p.gotCtx = ctx
	p.gotMessages = memory
	p.gotTools = avail
	// Exercise the stream channel so the drainer goroutine has something
	// to consume — catches an accidental unbuffered-chan deadlock.
	stream <- StreamEvent{Type: "thought", Content: "ok"}
	return p.result, p.err
}

func TestGenerateJSON_InstallsStructuredOutputOnCtx(t *testing.T) {
	p := &captureProvider{
		result: LLMResult{Content: `{"name":"Ada"}`},
	}
	req := GenerateJSONRequest{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
		Output: StructuredOutput{
			Name:   "person",
			Schema: map[string]any{"type": "object"},
		},
	}
	raw, _, err := GenerateJSON(context.Background(), p, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if raw != `{"name":"Ada"}` {
		t.Fatalf("raw: got %q", raw)
	}
	so := StructuredOutputFromContext(p.gotCtx)
	if so == nil || so.Name != "person" {
		t.Fatalf("StructuredOutput not installed on ctx: %+v", so)
	}
	if p.gotTools != nil {
		t.Fatalf("GenerateJSON must pass nil Registry (no tools), got %v", p.gotTools)
	}
}

func TestGenerateJSON_Into_DecodesIntoStruct(t *testing.T) {
	type Person struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	p := &captureProvider{
		result: LLMResult{Content: `{"name":"Ada","age":42}`},
	}
	req := GenerateJSONRequest{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
		Output: StructuredOutput{
			Schema: map[string]any{"type": "object"},
		},
	}
	var out Person
	if _, err := GenerateJSONInto(context.Background(), p, req, &out); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Name != "Ada" || out.Age != 42 {
		t.Fatalf("decode: got %+v", out)
	}
}

func TestGenerateJSON_Into_ReturnsDecodeError(t *testing.T) {
	type Person struct {
		Age int `json:"age"`
	}
	p := &captureProvider{
		result: LLMResult{Content: `{"age":"not-a-number"}`},
	}
	req := GenerateJSONRequest{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
		Output:   StructuredOutput{Schema: map[string]any{"type": "object"}},
	}
	var out Person
	if _, err := GenerateJSONInto(context.Background(), p, req, &out); err == nil {
		t.Fatal("expected decode error, got nil")
	}
}

func TestGenerateJSON_RejectsEmptyMessages(t *testing.T) {
	p := &captureProvider{}
	req := GenerateJSONRequest{
		Output: StructuredOutput{Schema: map[string]any{"type": "object"}},
	}
	if _, _, err := GenerateJSON(context.Background(), p, req); err == nil {
		t.Fatal("expected error on empty messages")
	}
}

func TestGenerateJSON_RejectsEmptySchema(t *testing.T) {
	p := &captureProvider{}
	req := GenerateJSONRequest{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
	}
	if _, _, err := GenerateJSON(context.Background(), p, req); err == nil {
		t.Fatal("expected error on empty schema")
	}
}

func TestGenerateJSON_RejectsNilProvider(t *testing.T) {
	req := GenerateJSONRequest{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
		Output:   StructuredOutput{Schema: map[string]any{"type": "object"}},
	}
	if _, _, err := GenerateJSON(context.Background(), nil, req); err == nil {
		t.Fatal("expected error on nil provider")
	}
}

func TestGenerateJSON_SurfacesProviderError(t *testing.T) {
	boom := errors.New("boom")
	p := &captureProvider{err: boom}
	req := GenerateJSONRequest{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
		Output:   StructuredOutput{Schema: map[string]any{"type": "object"}},
	}
	_, _, err := GenerateJSON(context.Background(), p, req)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("expected wrapped boom, got %v", err)
	}
}

func TestGenerateJSON_EmptyContentIsError(t *testing.T) {
	// An empty response with no tool calls is a bug on the provider's
	// side — surface it clearly instead of returning an empty string.
	p := &captureProvider{result: LLMResult{Content: ""}}
	req := GenerateJSONRequest{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
		Output:   StructuredOutput{Schema: map[string]any{"type": "object"}},
	}
	if _, _, err := GenerateJSON(context.Background(), p, req); err == nil {
		t.Fatal("expected error on empty content")
	}
}
