package gemini

import (
	"reflect"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
	"google.golang.org/genai"
)

type findGameArgs struct {
	Query   string   `json:"query" description:"Title or id"`
	Tags    []string `json:"tags" description:"Filter tags"`
	Limit   int      `json:"limit,omitempty"`
	Mode    string   `json:"mode" enum:"strict,fuzzy"`
	Filters struct {
		Genre  string   `json:"genre"`
		Stores []string `json:"stores"`
	} `json:"filters"`
	Players []struct {
		ID   string `json:"id"`
		Tier int    `json:"tier"`
	} `json:"players"`
}

func TestMapRegistrySchemaToGeminiSchema_FullCoverage(t *testing.T) {
	got := mapRegistrySchemaToGeminiSchema(tools.SchemaFor[findGameArgs]())
	if got == nil {
		t.Fatal("nil schema")
	}
	if got.Type != genai.TypeObject {
		t.Fatalf("root type = %v, want object", got.Type)
	}

	tags := got.Properties["tags"]
	if tags == nil || tags.Type != genai.TypeArray {
		t.Fatalf("tags missing or wrong type: %+v", tags)
	}
	if tags.Items == nil || tags.Items.Type != genai.TypeString {
		t.Fatalf("tags.Items should be string schema, got %+v", tags.Items)
	}
	if tags.Description != "Filter tags" {
		t.Fatalf("tags description = %q", tags.Description)
	}

	mode := got.Properties["mode"]
	if mode == nil || !reflect.DeepEqual(mode.Enum, []string{"strict", "fuzzy"}) {
		t.Fatalf("mode enum = %+v, want [strict fuzzy]", mode)
	}

	filters := got.Properties["filters"]
	if filters == nil || filters.Type != genai.TypeObject {
		t.Fatalf("filters wrong: %+v", filters)
	}
	if filters.Properties["genre"] == nil || filters.Properties["genre"].Type != genai.TypeString {
		t.Fatalf("filters.genre missing")
	}
	stores := filters.Properties["stores"]
	if stores == nil || stores.Type != genai.TypeArray || stores.Items == nil || stores.Items.Type != genai.TypeString {
		t.Fatalf("filters.stores nested array broken: %+v", stores)
	}
	if !reflect.DeepEqual(filters.Required, []string{"genre", "stores"}) {
		t.Fatalf("filters.Required = %+v, want [genre stores]", filters.Required)
	}

	players := got.Properties["players"]
	if players == nil || players.Type != genai.TypeArray || players.Items == nil {
		t.Fatalf("players missing items: %+v", players)
	}
	if players.Items.Type != genai.TypeObject {
		t.Fatalf("players.Items.Type = %v, want object", players.Items.Type)
	}
	if players.Items.Properties["id"] == nil || players.Items.Properties["tier"] == nil {
		t.Fatalf("players.Items.Properties missing id/tier: %+v", players.Items.Properties)
	}
	if players.Items.Properties["tier"].Type != genai.TypeInteger {
		t.Fatalf("players.Items.tier type = %v, want integer", players.Items.Properties["tier"].Type)
	}

	wantRequired := map[string]bool{"query": true, "tags": true, "mode": true, "filters": true, "players": true}
	if len(got.Required) != len(wantRequired) {
		t.Fatalf("root required = %+v, want keys %+v", got.Required, wantRequired)
	}
	for _, r := range got.Required {
		if !wantRequired[r] {
			t.Fatalf("unexpected required key %q (have %+v)", r, got.Required)
		}
	}
}

func TestPropMapToGeminiSchema_HandWrittenAnySlices(t *testing.T) {
	in := map[string]any{
		"type":     "object",
		"required": []any{"x"},
		"properties": map[string]any{
			"x": map[string]any{
				"type": "string",
				"enum": []any{"a", "b"},
			},
		},
	}
	got := propMapToGeminiSchema(in)
	if got == nil || got.Type != genai.TypeObject {
		t.Fatalf("root: %+v", got)
	}
	if !reflect.DeepEqual(got.Required, []string{"x"}) {
		t.Fatalf("required = %+v", got.Required)
	}
	x := got.Properties["x"]
	if x == nil || !reflect.DeepEqual(x.Enum, []string{"a", "b"}) {
		t.Fatalf("enum = %+v", x)
	}
}
