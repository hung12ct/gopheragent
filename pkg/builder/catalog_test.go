package builder

import (
	"context"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// tagTool wraps a tool and flips a flag on Execute, so a test can observe that
// catalog middleware actually wrapped the tool the builder registered.
type tagTool struct {
	tools.Tool
	onExec func()
}

func (t *tagTool) Execute(ctx context.Context, argsJSON string) (tools.Result, error) {
	t.onExec()
	return t.Tool.Execute(ctx, argsJSON)
}

func TestCatalog_UseAppliesMiddlewareToBuiltTools(t *testing.T) {
	catalog := NewGlobalCatalog()
	catalog.Register(&dummyTool{name: "web_search"})

	var wrapped bool
	catalog.Use(func(next tools.Tool) tools.Tool {
		return &tagTool{Tool: next, onExec: func() { wrapped = true }}
	})

	p := writeYAML(t, `
agent:
  name: "Test Agent"
  system_prompt: "You are helpful."
  tools_required:
    - "web_search"
`)
	loop, _, _, err := BuildFromYAML(p, catalog, nil, nil)
	if err != nil {
		t.Fatalf("BuildFromYAML: %v", err)
	}

	tool, ok := loop.Tools.Get("web_search")
	if !ok {
		t.Fatal("web_search missing from loop registry — middleware may have changed the name")
	}
	if _, err := tool.Execute(context.Background(), "{}"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !wrapped {
		t.Fatal("catalog middleware was not applied to the built tool")
	}
}

func TestCatalog_NoMiddlewareLeavesToolUnwrapped(t *testing.T) {
	catalog := NewGlobalCatalog()
	base := &dummyTool{name: "web_search"}
	catalog.Register(base)

	if got := catalog.wrap(base); got != tools.Tool(base) {
		t.Fatal("wrap should return the tool unchanged when no middleware is registered")
	}
}

func TestGlobalCatalog_ListNamesIsSorted(t *testing.T) {
	c := NewGlobalCatalog()
	for _, name := range []string{"zulu", "alpha", "mike", "bravo"} {
		tools.RegisterFunc(c, name, "desc",
			func(_ context.Context, _ struct{}) (tools.Result, error) { return tools.Text("ok"), nil })
	}
	want := []string{"alpha", "bravo", "mike", "zulu"}
	// Map iteration is randomized, so a single pass can pass by luck.
	for i := 0; i < 20; i++ {
		got := c.ListNames()
		if len(got) != len(want) {
			t.Fatalf("want %d names, got %v", len(want), got)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("iteration %d: got %v, want %v", i, got, want)
			}
		}
	}
}
