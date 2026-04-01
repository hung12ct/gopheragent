package tools

import (
	"context"
	"testing"
)

type stubTool struct{ name string }

func (s *stubTool) Name() string                          { return s.name }
func (s *stubTool) Description() string                   { return "stub" }
func (s *stubTool) ParametersSchema() ToolSchema { return ToolSchema{} }
func (s *stubTool) RequiresConfirmation() bool             { return false }
func (s *stubTool) Execute(_ context.Context, _ string) (string, error) { return "", nil }

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "alpha"})

	tool, ok := r.Get("alpha")
	if !ok || tool.Name() != "alpha" {
		t.Fatal("expected to find registered tool")
	}

	_, ok = r.Get("nonexistent")
	if ok {
		t.Fatal("expected false for missing tool")
	}
}

func TestRegistry_GetAll_DeterministicOrder(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "charlie"})
	r.Register(&stubTool{name: "alpha"})
	r.Register(&stubTool{name: "bravo"})

	all := r.GetAll()
	if len(all) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(all))
	}
	expected := []string{"alpha", "bravo", "charlie"}
	for i, tool := range all {
		if tool.Name() != expected[i] {
			t.Fatalf("expected %q at index %d, got %q", expected[i], i, tool.Name())
		}
	}
}

func TestRegistry_OverwriteByName(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "tool"})
	r.Register(&stubTool{name: "tool"}) // overwrite

	all := r.GetAll()
	if len(all) != 1 {
		t.Fatalf("expected 1 tool after overwrite, got %d", len(all))
	}
}
