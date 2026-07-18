package gemini

import (
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"google.golang.org/genai"
)

func TestApplyGeminiStructuredOutput_SetsMIMEAndSchema(t *testing.T) {
	cfg := &genai.GenerateContentConfig{}
	so := &agent.StructuredOutput{
		Name: "person",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []string{"name"},
		},
	}
	applyStructuredOutput(cfg, so)
	if cfg.ResponseMIMEType != "application/json" {
		t.Fatalf("mime: want application/json, got %q", cfg.ResponseMIMEType)
	}
	m, ok := cfg.ResponseJsonSchema.(map[string]any)
	if !ok {
		t.Fatalf("schema: want map[string]any, got %T", cfg.ResponseJsonSchema)
	}
	if m["type"] != "object" {
		t.Fatalf("schema.type: want object, got %v", m["type"])
	}
	// ResponseSchema must stay nil — Gemini rejects requests that set both.
	if cfg.ResponseSchema != nil {
		t.Fatalf("ResponseSchema must be left unset when ResponseJsonSchema is used")
	}
}

func TestApplyGeminiStructuredOutput_NilIsNoOp(t *testing.T) {
	cfg := &genai.GenerateContentConfig{}
	applyStructuredOutput(cfg, nil)
	if cfg.ResponseMIMEType != "" {
		t.Fatalf("nil so must leave MIME empty, got %q", cfg.ResponseMIMEType)
	}
	if cfg.ResponseJsonSchema != nil {
		t.Fatalf("nil so must leave schema nil, got %v", cfg.ResponseJsonSchema)
	}
}

func TestApplyGeminiStructuredOutput_EmptySchemaIsNoOp(t *testing.T) {
	// Defensive: an empty map should behave like nil — Gemini rejects
	// response_mime_type without an accompanying schema.
	cfg := &genai.GenerateContentConfig{}
	applyStructuredOutput(cfg, &agent.StructuredOutput{Schema: map[string]any{}})
	if cfg.ResponseMIMEType != "" || cfg.ResponseJsonSchema != nil {
		t.Fatalf("empty schema must be no-op, got mime=%q schema=%v",
			cfg.ResponseMIMEType, cfg.ResponseJsonSchema)
	}
}
