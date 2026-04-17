package tools

import (
	"reflect"
	"testing"
)

func TestSchemaFor_Primitives(t *testing.T) {
	type Args struct {
		Name   string  `json:"name" description:"the name"`
		Age    int     `json:"age"`
		Height float64 `json:"height"`
		Active bool    `json:"active"`
	}

	got := SchemaFor[Args]()

	if got.Type != "object" {
		t.Fatalf("expected type=object, got %q", got.Type)
	}
	cases := map[string]string{"name": "string", "age": "integer", "height": "number", "active": "boolean"}
	for field, wantType := range cases {
		p, ok := got.Properties[field].(map[string]any)
		if !ok {
			t.Fatalf("missing property %q: %+v", field, got.Properties)
		}
		if p["type"] != wantType {
			t.Fatalf("field %q: expected type %q, got %v", field, wantType, p["type"])
		}
	}
	if p, _ := got.Properties["name"].(map[string]any); p["description"] != "the name" {
		t.Fatalf("expected description on 'name', got %+v", p)
	}
	// All four fields required by default (declaration order).
	if !reflect.DeepEqual(got.Required, []string{"name", "age", "height", "active"}) {
		t.Fatalf("expected all fields required in declaration order, got %v", got.Required)
	}
}

func TestSchemaFor_OmitemptyAndPointerAreOptional(t *testing.T) {
	type Args struct {
		Required string  `json:"required"`
		Optional string  `json:"optional,omitempty"`
		Maybe    *string `json:"maybe"`
	}

	got := SchemaFor[Args]()

	if !reflect.DeepEqual(got.Required, []string{"required"}) {
		t.Fatalf("expected only 'required' in Required, got %v", got.Required)
	}
	if _, ok := got.Properties["maybe"]; !ok {
		t.Fatalf("pointer field should still appear in properties")
	}
}

func TestSchemaFor_RequiredTagOverrides(t *testing.T) {
	type Args struct {
		Forced   string `json:"forced,omitempty" required:"true"`
		Released string `json:"released" required:"false"`
	}

	got := SchemaFor[Args]()

	if !reflect.DeepEqual(got.Required, []string{"forced"}) {
		t.Fatalf("expected Required=[forced], got %v", got.Required)
	}
}

func TestSchemaFor_EnumTag(t *testing.T) {
	type Args struct {
		Topic string `json:"topic" enum:"general, news , other"`
	}

	got := SchemaFor[Args]()

	p := got.Properties["topic"].(map[string]any)
	enum, ok := p["enum"].([]string)
	if !ok {
		t.Fatalf("expected enum to be []string, got %T", p["enum"])
	}
	if !reflect.DeepEqual(enum, []string{"general", "news", "other"}) {
		t.Fatalf("expected trimmed enum values, got %v", enum)
	}
}

func TestSchemaFor_JSONDashSkipsField(t *testing.T) {
	type Args struct {
		Visible string `json:"visible"`
		Hidden  string `json:"-"`
	}

	got := SchemaFor[Args]()

	if _, ok := got.Properties["visible"]; !ok {
		t.Fatalf("visible field should be present")
	}
	if _, ok := got.Properties["Hidden"]; ok {
		t.Fatalf("json:\"-\" field should be skipped")
	}
	if _, ok := got.Properties["-"]; ok {
		t.Fatalf("json:\"-\" field should not appear under '-' key either")
	}
}

func TestSchemaFor_UnexportedFieldsSkipped(t *testing.T) {
	type Args struct {
		Public  string `json:"public"`
		private string //nolint:unused
	}

	got := SchemaFor[Args]()

	if len(got.Properties) != 1 {
		t.Fatalf("expected exactly 1 property, got %d: %+v", len(got.Properties), got.Properties)
	}
}

func TestSchemaFor_SliceOfPrimitives(t *testing.T) {
	type Args struct {
		Tags []string `json:"tags" description:"labels"`
	}

	got := SchemaFor[Args]()

	p := got.Properties["tags"].(map[string]any)
	if p["type"] != "array" {
		t.Fatalf("expected array type, got %v", p["type"])
	}
	item, ok := p["items"].(map[string]any)
	if !ok {
		t.Fatalf("expected items to be map, got %T", p["items"])
	}
	if item["type"] != "string" {
		t.Fatalf("expected item type string, got %v", item["type"])
	}
	if p["description"] != "labels" {
		t.Fatalf("expected description to be preserved on slice, got %v", p["description"])
	}
}

func TestSchemaFor_NestedStruct(t *testing.T) {
	type Address struct {
		City string `json:"city"`
		Zip  string `json:"zip,omitempty"`
	}
	type Args struct {
		Name    string  `json:"name"`
		Address Address `json:"address"`
	}

	got := SchemaFor[Args]()

	addr := got.Properties["address"].(map[string]any)
	if addr["type"] != "object" {
		t.Fatalf("expected nested type=object, got %v", addr["type"])
	}
	props := addr["properties"].(map[string]any)
	if _, ok := props["city"]; !ok {
		t.Fatalf("nested property 'city' missing")
	}
	nestedReq := addr["required"].([]string)
	if !reflect.DeepEqual(nestedReq, []string{"city"}) {
		t.Fatalf("expected nested required=[city], got %v", nestedReq)
	}
}

func TestSchemaFor_PointerGenericUnwraps(t *testing.T) {
	type Args struct {
		Name string `json:"name"`
	}

	got := SchemaFor[*Args]()

	if got.Type != "object" {
		t.Fatalf("expected object schema when T is *Struct, got %q", got.Type)
	}
	if _, ok := got.Properties["name"]; !ok {
		t.Fatalf("expected 'name' property")
	}
}

func TestSchemaFor_PanicsOnNonStruct(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for non-struct type")
		}
	}()
	_ = SchemaFor[string]()
}

func TestSchemaFor_PanicsOnUnsupportedField(t *testing.T) {
	type Args struct {
		Bad map[string]int `json:"bad"`
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for unsupported field kind")
		}
	}()
	_ = SchemaFor[Args]()
}

func TestSchemaFor_FallbackToFieldNameWhenNoJSONTag(t *testing.T) {
	type Args struct {
		Query string
	}

	got := SchemaFor[Args]()

	if _, ok := got.Properties["Query"]; !ok {
		t.Fatalf("expected fallback to Go field name 'Query', got %+v", got.Properties)
	}
}
