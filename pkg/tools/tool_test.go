package tools

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
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

type raceProbeTool struct{ name string }

func (n raceProbeTool) Descriptor() ToolDescriptor { return ToolDescriptor{Name: n.name} }
func (n raceProbeTool) Execute(context.Context, string) (Result, error) {
	return Text("ok"), nil
}

// Debug mode used to build its wrappers lazily inside Get/All, which hold
// only a read lock — a shared lock, so concurrent lookups raced on the map
// and could trip a concurrent map write. The loop dispatches tools in
// parallel, and debug mode is what you turn on to investigate that.
func TestRegistry_DebugModeConcurrentAccessIsRaceFree(t *testing.T) {
	reg := NewRegistry()
	reg.EnableDebug(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, n := range []string{"a", "b", "c", "d"} {
		reg.Register(raceProbeTool{name: n})
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, ok := reg.Get("a"); !ok {
				t.Errorf("Get(a) failed")
			}
			if got := len(reg.All()); got != 4 {
				t.Errorf("All() returned %d tools", got)
			}
			// Registering concurrently exercises the write path too.
			if i%10 == 0 {
				reg.Register(raceProbeTool{name: "a"})
			}
		}(i)
	}
	wg.Wait()
}

// Wrapping must survive both orderings: tools registered before debug is
// enabled, and after.
func TestRegistry_DebugWrapsRegardlessOfOrder(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	reg := NewRegistry()
	reg.Register(raceProbeTool{name: "before"})
	reg.EnableDebug(logger)
	reg.Register(raceProbeTool{name: "after"})

	for _, name := range []string{"before", "after"} {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		if _, err := tool.Execute(context.Background(), "{}"); err != nil {
			t.Fatalf("execute %s: %v", name, err)
		}
		if !strings.Contains(buf.String(), name) {
			t.Fatalf("debug wrapper did not log for %q registered %s enabling debug: %q", name, name, buf.String())
		}
		buf.Reset()
	}

	// A clone must keep its wrappers rather than silently losing them.
	clone := reg.Clone()
	tool, _ := clone.Get("before")
	if _, err := tool.Execute(context.Background(), "{}"); err != nil {
		t.Fatalf("clone execute: %v", err)
	}
	if !strings.Contains(buf.String(), "before") {
		t.Fatalf("clone lost debug wrapping: %q", buf.String())
	}
}
