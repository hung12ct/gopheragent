package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func TestStructuredOutput_EmptyCtxReturnsNil(t *testing.T) {
	if got := StructuredOutputFromContext(context.Background()); got != nil {
		t.Fatalf("empty ctx: want nil, got %+v", got)
	}
}

func TestStructuredOutput_RoundTrip(t *testing.T) {
	so := StructuredOutput{
		Name:        "person",
		Description: "a human",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
				"age":  map[string]any{"type": "integer"},
			},
			"required": []string{"name"},
		},
		Strict: true,
	}
	ctx := WithStructuredOutput(context.Background(), so)
	got := StructuredOutputFromContext(ctx)
	if got == nil {
		t.Fatal("want non-nil, got nil")
	}
	if got.Name != "person" || got.Description != "a human" || !got.Strict {
		t.Fatalf("fields lost: %+v", got)
	}
	if _, ok := got.Schema["properties"]; !ok {
		t.Fatalf("schema properties lost: %+v", got.Schema)
	}
}

func TestStructuredOutput_IsolatedFromOriginal(t *testing.T) {
	// The helper must store a copy so post-hoc mutation of the caller's
	// struct cannot silently change the value on ctx.
	so := StructuredOutput{
		Name:   "a",
		Schema: map[string]any{"type": "string"},
	}
	ctx := WithStructuredOutput(context.Background(), so)
	so.Name = "b"
	got := StructuredOutputFromContext(ctx)
	if got.Name != "a" {
		t.Fatalf("ctx value must not alias caller: got %q", got.Name)
	}
}

func TestStructuredOutput_EmptyOrNilSchemaClears(t *testing.T) {
	cases := []StructuredOutput{
		{},                      // zero value
		{Name: "x"},             // no schema
		{Schema: map[string]any{}}, // empty schema
	}
	for i, so := range cases {
		ctx := WithStructuredOutput(context.Background(), so)
		if got := StructuredOutputFromContext(ctx); got != nil {
			t.Fatalf("case %d: empty schema must clear hint, got %+v", i, got)
		}
	}
}

func TestStructuredOutput_OverwritesEarlierValue(t *testing.T) {
	ctx := WithStructuredOutput(context.Background(), StructuredOutput{
		Name:   "first",
		Schema: map[string]any{"type": "string"},
	})
	ctx = WithStructuredOutput(ctx, StructuredOutput{
		Name:   "second",
		Schema: map[string]any{"type": "integer"},
	})
	got := StructuredOutputFromContext(ctx)
	if got == nil || got.Name != "second" {
		t.Fatalf("want latest value 'second', got %+v", got)
	}
}

func TestStructuredOutput_WrongTypeValueIgnored(t *testing.T) {
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, "unrelated")
	if got := StructuredOutputFromContext(ctx); got != nil {
		t.Fatalf("unrelated ctx value must not leak: got %+v", got)
	}
}

func TestStructuredOutput_MarshalSchema(t *testing.T) {
	so := &StructuredOutput{
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"x": map[string]any{"type": "number"},
			},
		},
	}
	raw, err := so.MarshalSchema()
	if err != nil {
		t.Fatalf("MarshalSchema: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("round-trip JSON: %v", err)
	}
	if back["type"] != "object" {
		t.Fatalf("type round-trip: got %v", back["type"])
	}
}

func TestStructuredOutput_MarshalSchema_NilSafe(t *testing.T) {
	var so *StructuredOutput
	raw, err := so.MarshalSchema()
	if err != nil || raw != nil {
		t.Fatalf("nil receiver: want (nil, nil), got (%v, %v)", raw, err)
	}
	empty := &StructuredOutput{}
	raw, err = empty.MarshalSchema()
	if err != nil || raw != nil {
		t.Fatalf("empty schema: want (nil, nil), got (%v, %v)", raw, err)
	}
}
