package tools

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
)

type describedTool struct {
	name string
	desc string
}

func (t *describedTool) Name() string                                    { return t.name }
func (t *describedTool) Description() string                             { return t.desc }
func (t *describedTool) ParametersSchema() ToolSchema                    { return ToolSchema{} }
func (t *describedTool) RequiresConfirmation() bool                      { return false }
func (t *describedTool) Display() ToolDisplay { return DefaultDisplay(t.Name(), t.Description()) }
func (t *describedTool) Execute(_ context.Context, _ string) (string, error) { return "", nil }

// keywordEmbedder produces 3-dim vectors keyed by hand-picked keywords. Gives
// deterministic, testable cosine similarity without hitting a real embedder.
type keywordEmbedder struct {
	err   error
	calls int
}

func (e *keywordEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		t = strings.ToLower(t)
		var v [3]float32
		if strings.Contains(t, "weather") || strings.Contains(t, "forecast") {
			v[0] = 1
		}
		if strings.Contains(t, "order") || strings.Contains(t, "inventory") {
			v[1] = 1
		}
		if strings.Contains(t, "image") || strings.Contains(t, "picture") {
			v[2] = 1
		}
		out[i] = v[:]
	}
	return out, nil
}

func TestCosineSimilarity(t *testing.T) {
	cases := []struct {
		name   string
		a, b   []float32
		expect float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0},
		{"orthogonal", []float32{1, 0, 0}, []float32{0, 1, 0}, 0.0},
		{"opposite", []float32{1, 0, 0}, []float32{-1, 0, 0}, -1.0},
		{"mismatched-len", []float32{1, 0}, []float32{1, 0, 0}, 0.0},
		{"zero-magnitude", []float32{0, 0, 0}, []float32{1, 0, 0}, 0.0},
		{"empty", []float32{}, []float32{}, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cosineSimilarity(tc.a, tc.b)
			if math.Abs(got-tc.expect) > 1e-6 {
				t.Fatalf("expected %v, got %v", tc.expect, got)
			}
		})
	}
}

func TestSelector_TopK(t *testing.T) {
	r := NewRegistry()
	r.Register(&describedTool{name: "get_weather", desc: "returns the current weather forecast"})
	r.Register(&describedTool{name: "lookup_order", desc: "fetches an order by id from inventory"})
	r.Register(&describedTool{name: "show_image", desc: "renders an image or picture inline"})

	emb := &keywordEmbedder{}
	sel, err := NewSelector(context.Background(), r, emb, 1)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("expected 1 embedder call at init, got %d", emb.calls)
	}

	got, err := sel.Select(context.Background(), "what is the weather like today?")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 1 || got[0].Name() != "get_weather" {
		names := make([]string, len(got))
		for i, t := range got {
			names[i] = t.Name()
		}
		t.Fatalf("expected [get_weather], got %v", names)
	}
}

func TestSelector_Pinned(t *testing.T) {
	r := NewRegistry()
	r.Register(&describedTool{name: "get_weather", desc: "returns the weather forecast"})
	r.Register(&describedTool{name: "lookup_order", desc: "fetches an order"})
	r.Register(&describedTool{name: "final_answer", desc: "tool that never matches any keyword"})

	emb := &keywordEmbedder{}
	sel, err := NewSelector(context.Background(), r, emb, 1, WithPinned("final_answer"))
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}

	got, err := sel.Select(context.Background(), "forecast for tomorrow")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	names := toolNames(got)
	if !contains(names, "get_weather") || !contains(names, "final_answer") {
		t.Fatalf("expected [get_weather, final_answer], got %v", names)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 tools (top-1 + pinned), got %d: %v", len(got), names)
	}
}

func TestSelector_EmptyQueryReturnsAll(t *testing.T) {
	r := NewRegistry()
	r.Register(&describedTool{name: "a", desc: "weather"})
	r.Register(&describedTool{name: "b", desc: "order"})

	sel, err := NewSelector(context.Background(), r, &keywordEmbedder{}, 1)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	got, err := sel.Select(context.Background(), "")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected all tools for empty query, got %d", len(got))
	}
}

func TestSelector_TopKLargerThanRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register(&describedTool{name: "a", desc: "weather"})

	sel, err := NewSelector(context.Background(), r, &keywordEmbedder{}, 100)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	got, _ := sel.Select(context.Background(), "weather")
	if len(got) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(got))
	}
}

func TestSelector_EmptyRegistry(t *testing.T) {
	r := NewRegistry()
	sel, err := NewSelector(context.Background(), r, &keywordEmbedder{}, 3)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	got, err := sel.Select(context.Background(), "anything")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for empty registry, got %v", got)
	}
}

func TestSelector_EmbedError(t *testing.T) {
	r := NewRegistry()
	r.Register(&describedTool{name: "a", desc: "x"})
	emb := &keywordEmbedder{err: errors.New("boom")}
	_, err := NewSelector(context.Background(), r, emb, 1)
	if err == nil {
		t.Fatalf("expected init error when embedder fails")
	}
}

func TestSelector_NilArgs(t *testing.T) {
	if _, err := NewSelector(context.Background(), nil, &keywordEmbedder{}, 1); err == nil {
		t.Fatal("expected error for nil registry")
	}
	if _, err := NewSelector(context.Background(), NewRegistry(), nil, 1); err == nil {
		t.Fatal("expected error for nil embedder")
	}
}

func TestSelector_Refresh(t *testing.T) {
	r := NewRegistry()
	r.Register(&describedTool{name: "a", desc: "weather"})
	emb := &keywordEmbedder{}
	sel, err := NewSelector(context.Background(), r, emb, 1)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}

	r.Register(&describedTool{name: "b", desc: "order"})
	// Without Refresh, b is not in the embedding set — query matching "order"
	// finds nothing in the ranked list, falls back to top-K of known tools.
	if err := sel.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got, _ := sel.Select(context.Background(), "order status")
	if len(got) != 1 || got[0].Name() != "b" {
		t.Fatalf("expected [b] after Refresh, got %v", toolNames(got))
	}
}

func TestSelectRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register(&describedTool{name: "get_weather", desc: "weather forecast"})
	r.Register(&describedTool{name: "lookup_order", desc: "order by id"})

	sel, err := NewSelector(context.Background(), r, &keywordEmbedder{}, 1)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	filtered, err := sel.SelectRegistry(context.Background(), "order status")
	if err != nil {
		t.Fatalf("SelectRegistry: %v", err)
	}
	all := filtered.GetAll()
	if len(all) != 1 || all[0].Name() != "lookup_order" {
		t.Fatalf("expected [lookup_order], got %v", toolNames(all))
	}
}

func toolNames(ts []Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name()
	}
	return out
}

func contains(hay []string, needle string) bool {
	return slices.Contains(hay, needle)
}
