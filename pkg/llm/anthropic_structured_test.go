package llm

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
)

func TestSynthesizeAnthropicStructuredTool_BasicShape(t *testing.T) {
	so := &agent.StructuredOutput{
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
	}
	tool, name := synthesizeAnthropicStructuredTool(so)
	if name != "person" {
		t.Fatalf("name: want person, got %q", name)
	}
	if tool.OfTool == nil {
		t.Fatal("OfTool is nil")
	}
	if tool.OfTool.Name != "person" {
		t.Fatalf("tool.Name: got %q", tool.OfTool.Name)
	}
	if !reflect.DeepEqual(tool.OfTool.InputSchema.Required, []string{"name"}) {
		t.Fatalf("Required: got %v", tool.OfTool.InputSchema.Required)
	}
	props, ok := tool.OfTool.InputSchema.Properties.(map[string]any)
	if !ok {
		t.Fatalf("Properties type: got %T", tool.OfTool.InputSchema.Properties)
	}
	if _, ok := props["name"]; !ok {
		t.Fatalf("Properties missing 'name': %v", props)
	}
}

func TestSynthesizeAnthropicStructuredTool_FallbackName(t *testing.T) {
	so := &agent.StructuredOutput{
		Schema: map[string]any{"type": "object"},
	}
	_, name := synthesizeAnthropicStructuredTool(so)
	if name == "" {
		t.Fatal("expected non-empty fallback name, got empty")
	}
}

func TestSynthesizeAnthropicStructuredTool_RequiredFromAnyList(t *testing.T) {
	// JSON-decoded schemas come back with []any for "required"; the helper
	// must accept both that and the more ergonomic []string form.
	so := &agent.StructuredOutput{
		Schema: map[string]any{
			"type":     "object",
			"required": []any{"a", "b", 42, "c"}, // 42 should be filtered out
		},
	}
	tool, _ := synthesizeAnthropicStructuredTool(so)
	got := tool.OfTool.InputSchema.Required
	sort.Strings(got)
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Required: got %v, want %v", got, want)
	}
}

func TestSynthesizeAnthropicStructuredTool_ExtrasPreserved(t *testing.T) {
	// Uncommon JSON-Schema keywords must round-trip via ExtraFields so
	// callers can use additionalProperties / $defs / etc.
	so := &agent.StructuredOutput{
		Schema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"$defs": map[string]any{
				"Point": map[string]any{"type": "object"},
			},
		},
	}
	tool, _ := synthesizeAnthropicStructuredTool(so)
	if _, ok := tool.OfTool.InputSchema.ExtraFields["additionalProperties"]; !ok {
		t.Fatalf("additionalProperties not preserved in ExtraFields: %+v", tool.OfTool.InputSchema.ExtraFields)
	}
	if _, ok := tool.OfTool.InputSchema.ExtraFields["$defs"]; !ok {
		t.Fatalf("$defs not preserved: %+v", tool.OfTool.InputSchema.ExtraFields)
	}
	// Marshal the schema and confirm the extras land at the top level.
	raw, err := json.Marshal(tool.OfTool.InputSchema)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["additionalProperties"] != false {
		t.Fatalf("additionalProperties lost on marshal: %v", back)
	}
}

func TestSynthesizeAnthropicStructuredTool_DefaultDescription(t *testing.T) {
	// Anthropic tool descriptions materially affect model behavior, so a
	// blank description should fall back to something instructive.
	so := &agent.StructuredOutput{
		Schema: map[string]any{"type": "object"},
	}
	tool, _ := synthesizeAnthropicStructuredTool(so)
	if tool.OfTool.Description.Value == "" {
		t.Fatal("expected non-empty fallback description")
	}
}
