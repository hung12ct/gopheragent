package toolsfake_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
	"github.com/hung12ct/gopheragent/pkg/tools/toolsfake"
)

// Compile-time interface satisfaction check.
var _ tools.Tool = (*toolsfake.Tool)(nil)

func TestTool_DefaultsAndFluentConfig(t *testing.T) {
	tool := toolsfake.NewTool("echo").
		WithDescription("echoes back").
		WithConfirmation(true).
		WithResult(`"ok"`)

	if tool.Name() != "echo" {
		t.Errorf("Name() = %q, want echo", tool.Name())
	}
	if tool.Description() != "echoes back" {
		t.Errorf("Description() = %q", tool.Description())
	}
	if !tool.RequiresConfirmation() {
		t.Error("expected RequiresConfirmation() true")
	}
	out, err := tool.Execute(context.Background(), `{"a":1}`)
	if err != nil {
		t.Fatalf("execute err: %v", err)
	}
	if out != `"ok"` {
		t.Errorf("out = %q", out)
	}
}

func TestTool_RecordsCallsAndArgs(t *testing.T) {
	tool := toolsfake.NewTool("x").WithResult("done")
	_, _ = tool.Execute(context.Background(), `{"i":0}`)
	_, _ = tool.Execute(context.Background(), `{"i":1}`)
	if tool.Calls() != 2 {
		t.Fatalf("expected 2 calls, got %d", tool.Calls())
	}
	if tool.LastArgs() != `{"i":1}` {
		t.Errorf("LastArgs = %q", tool.LastArgs())
	}
	if got := tool.AllArgs(); len(got) != 2 || got[0] != `{"i":0}` || got[1] != `{"i":1}` {
		t.Errorf("AllArgs = %+v", got)
	}
}

func TestTool_WithError(t *testing.T) {
	sentinel := errors.New("nope")
	tool := toolsfake.NewTool("x").WithError(sentinel)
	_, err := tool.Execute(context.Background(), `{}`)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
}

func TestTool_ResultFnOverridesStatic(t *testing.T) {
	tool := toolsfake.NewTool("x").WithResult("static")
	tool.ResultFn = func(args string) (string, error) { return "dyn:" + args, nil }
	out, _ := tool.Execute(context.Background(), "A")
	if out != "dyn:A" {
		t.Errorf("ResultFn should override Result; got %q", out)
	}
}

func TestTool_ConcurrentSafe(t *testing.T) {
	tool := toolsfake.NewTool("x").WithResult("ok")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = tool.Execute(context.Background(), "a")
		}()
	}
	wg.Wait()
	if tool.Calls() != 50 {
		t.Fatalf("expected 50 calls, got %d", tool.Calls())
	}
	if len(tool.AllArgs()) != 50 {
		t.Fatalf("expected 50 recorded args, got %d", len(tool.AllArgs()))
	}
}
