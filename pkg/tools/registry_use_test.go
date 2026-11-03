package tools

import (
	"context"
	"testing"
)

type countTool struct {
	Tool
	calls *int
}

func (c countTool) Execute(ctx context.Context, args string) (Result, error) {
	*c.calls++
	return c.Tool.Execute(ctx, args)
}

type plainTool struct{ name string }

func (p plainTool) Descriptor() ToolDescriptor { return ToolDescriptor{Name: p.name} }
func (p plainTool) Execute(context.Context, string) (Result, error) {
	return Text("ok"), nil
}

func TestRegistryUse_WrapsEveryRegisteredTool(t *testing.T) {
	r := NewRegistry()
	r.Register(plainTool{name: "a"})
	r.Register(plainTool{name: "b"})
	calls := 0
	r.Use(func(next Tool) Tool { return countTool{Tool: next, calls: &calls} })
	r.Register(plainTool{name: "late"})

	for _, name := range []string{"a", "b", "late"} {
		tool, ok := r.Get(name)
		if !ok {
			t.Fatalf("%s missing after Use — middleware must preserve names", name)
		}
		_, _ = tool.Execute(context.Background(), "{}")
	}
	if calls != 2 {
		t.Fatalf("middleware ran %d times, want 2 (tools registered after Use stay unwrapped)", calls)
	}
}

func TestRegistryUse_DebugModeWrapsRawTools(t *testing.T) {
	r := NewRegistry()
	r.EnableDebug(nil)
	r.Register(plainTool{name: "a"})
	calls := 0
	r.Use(func(next Tool) Tool { return countTool{Tool: next, calls: &calls} })
	tool, _ := r.Get("a")
	_, _ = tool.Execute(context.Background(), "{}")
	if calls != 1 {
		t.Fatalf("middleware ran %d times in debug mode, want 1", calls)
	}
}
