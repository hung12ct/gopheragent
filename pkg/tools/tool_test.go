package tools

import (
	"context"
	"testing"
)

type stubTool struct{ name string }

func (s *stubTool) Descriptor() ToolDescriptor {
	return ToolDescriptor{
		Name:        s.name,
		Description: "stub",
		Display:     DefaultDisplay(s.name, "stub"),
	}
}

func (s *stubTool) Execute(_ context.Context, _ string) (Result, error) { return Result{}, nil }

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "alpha"})

	tool, ok := r.Get("alpha")
	if !ok || tool.Descriptor().Name != "alpha" {
		t.Fatal("expected to find registered tool")
	}

	_, ok = r.Get("nonexistent")
	if ok {
		t.Fatal("expected false for missing tool")
	}
}

func TestRegistry_All_DeterministicOrder(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "charlie"})
	r.Register(&stubTool{name: "alpha"})
	r.Register(&stubTool{name: "bravo"})

	all := r.All()
	if len(all) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(all))
	}
	expected := []string{"alpha", "bravo", "charlie"}
	for i, tool := range all {
		if tool.Descriptor().Name != expected[i] {
			t.Fatalf("expected %q at index %d, got %q", expected[i], i, tool.Descriptor().Name)
		}
	}
}

func TestRegistry_OverwriteByName(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "tool"})
	r.Register(&stubTool{name: "tool"}) // overwrite

	all := r.All()
	if len(all) != 1 {
		t.Fatalf("expected 1 tool after overwrite, got %d", len(all))
	}
}

func TestReportProgress_NoOpWithoutInjection(t *testing.T) {
	// Safe to call on a plain context — must not panic, must not block.
	ReportProgress(context.Background(), "ignored")
}

func TestReportProgress_InvokesInjectedFunc(t *testing.T) {
	var got []string
	ctx := WithProgressFunc(context.Background(), func(msg string) {
		got = append(got, msg)
	})
	ReportProgress(ctx, "one")
	ReportProgress(ctx, "two")

	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("expected [one two], got %v", got)
	}
}

func TestReportProgress_NilFuncIsNoOp(t *testing.T) {
	// Passing a nil func should not panic when ReportProgress is later called.
	ctx := WithProgressFunc(context.Background(), nil)
	ReportProgress(ctx, "still safe")
}

func TestWithProgressFunc_Chaining(t *testing.T) {
	// Inner injection overrides outer — standard context.WithValue semantics.
	var outer, inner int
	ctx := WithProgressFunc(context.Background(), func(string) { outer++ })
	ctx = WithProgressFunc(ctx, func(string) { inner++ })
	ReportProgress(ctx, "m")
	if inner != 1 || outer != 0 {
		t.Fatalf("expected inner=1 outer=0, got inner=%d outer=%d", inner, outer)
	}
}
