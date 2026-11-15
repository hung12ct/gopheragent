package anthropic

import (
	"reflect"
	"testing"
)

func TestStructuredSchema_ClosesEveryObject(t *testing.T) {
	// The API rejects any object without additionalProperties:false, at
	// every depth: properties, array items, $defs and anyOf branches.
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tags": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "properties": map[string]any{"t": map[string]any{"type": "string"}},
			}},
			"either": map[string]any{"anyOf": []any{
				map[string]any{"type": "object", "additionalProperties": true},
				map[string]any{"type": "string"},
			}},
		},
		"$defs": map[string]any{"Point": map[string]any{"type": []any{"object", "null"}}},
	}
	out := structuredSchema(in)
	props := out["properties"].(map[string]any)
	item := props["tags"].(map[string]any)["items"].(map[string]any)
	branches := props["either"].(map[string]any)["anyOf"].([]any)
	point := out["$defs"].(map[string]any)["Point"].(map[string]any)
	for name, obj := range map[string]map[string]any{"root": out, "item": item, "anyOf": branches[0].(map[string]any), "$defs": point} {
		if obj["additionalProperties"] != false {
			t.Fatalf("%s: additionalProperties must be false, got %v", name, obj["additionalProperties"])
		}
	}
	if _, ok := branches[1].(map[string]any)["additionalProperties"]; ok {
		t.Fatal("a string schema must not gain additionalProperties")
	}
}

func TestStructuredSchema_MovesUnsupportedConstraintsIntoDescription(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"age":  map[string]any{"type": "integer", "minimum": 0, "maximum": 150, "description": "years"},
			"tags": map[string]any{"type": "array", "minItems": 2, "maxItems": 5, "items": map[string]any{"type": "string"}},
			"one":  map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}},
			"when": map[string]any{"type": "string", "format": "date"},
			"host": map[string]any{"type": "string", "format": "idn-hostname"},
		},
	}
	props := structuredSchema(in)["properties"].(map[string]any)
	age := props["age"].(map[string]any)
	want := map[string]any{"type": "integer", "description": "years {maximum: 150, minimum: 0}"}
	if !reflect.DeepEqual(age, want) {
		t.Fatalf("age: want %v, got %v", want, age)
	}
	tags := props["tags"].(map[string]any)
	if tags["description"] != "{maxItems: 5, minItems: 2}" || tags["minItems"] != nil || tags["maxItems"] != nil {
		t.Fatalf("tags: constraints must move to the description, got %v", tags)
	}
	if one := props["one"].(map[string]any); one["minItems"] != 1 {
		t.Fatalf("minItems 1 is supported and must stay, got %v", one)
	}
	if when := props["when"].(map[string]any); when["format"] != "date" {
		t.Fatalf("a supported format must stay, got %v", when)
	}
	if host := props["host"].(map[string]any); host["format"] != nil || host["description"] != "{format: idn-hostname}" {
		t.Fatalf("an unsupported format must move to the description, got %v", host)
	}
}

func TestStructuredSchema_LeavesCallerSchemaUntouched(t *testing.T) {
	age := map[string]any{"type": "integer", "minimum": 0}
	in := map[string]any{"type": "object", "properties": map[string]any{"age": age}}
	structuredSchema(in)
	if _, ok := in["additionalProperties"]; ok {
		t.Fatal("the caller's root schema was mutated")
	}
	if age["minimum"] != 0 || age["description"] != nil {
		t.Fatalf("the caller's nested schema was mutated: %v", age)
	}
}
