// Package toolsfake provides a configurable tools.Tool fake for agent tests.
// It records every invocation (args, count) and returns a scripted result or
// error, so tests can assert the loop drove the tool with the expected
// arguments without hand-rolling a stub each time.
//
// Typical use:
//
//	fakeTool := toolsfake.NewTool("echo").
//	    WithResult(`"echo result"`).
//	    WithConfirmation(false)
//	reg := tools.NewRegistry()
//	reg.Register(fakeTool)
//	// ... run the loop ...
//	if fakeTool.Calls() != 1 {
//	    t.Fatalf("expected 1 call, got %d", fakeTool.Calls())
//	}
//	if got := fakeTool.LastArgs(); got != expected {
//	    t.Errorf("unexpected args: %s", got)
//	}
package toolsfake

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// Tool is a fake tools.Tool suitable for tests. Safe for concurrent use.
type Tool struct {
	name        string
	description string
	schema      tools.ToolSchema
	confirm     bool

	// ResultFn, when non-nil, produces the Execute return value per-call.
	// When nil, the tool returns Result / Err unchanged every call.
	ResultFn func(argsJSON string) (string, error)
	Result   string
	Err      error

	calls atomic.Int64

	mu       sync.Mutex
	lastArgs string
	allArgs  []string
}

// NewTool returns a new fake tool with the given Name. Description defaults
// to "fake tool", schema defaults to an empty object. Chain the With*
// methods to configure further.
func NewTool(name string) *Tool {
	return &Tool{
		name:        name,
		description: "fake tool",
		schema:      tools.ToolSchema{Type: "object"},
	}
}

// WithDescription sets the Description returned by the tool.
func (t *Tool) WithDescription(desc string) *Tool {
	t.description = desc
	return t
}

// WithSchema sets the ToolSchema returned by ParametersSchema.
func (t *Tool) WithSchema(s tools.ToolSchema) *Tool {
	t.schema = s
	return t
}

// WithConfirmation flips RequiresConfirmation. Default false.
func (t *Tool) WithConfirmation(b bool) *Tool {
	t.confirm = b
	return t
}

// WithResult sets a fixed result returned from every Execute call.
func (t *Tool) WithResult(r string) *Tool {
	t.Result = r
	return t
}

// WithError sets a fixed error returned from every Execute call.
func (t *Tool) WithError(err error) *Tool {
	t.Err = err
	return t
}

// Descriptor implements tools.Tool. Returns the configured metadata block
// (name, description, schema, RequiresConfirmation flag, display).
func (t *Tool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:                 t.name,
		Description:          t.description,
		Parameters:           t.schema,
		RequiresConfirmation: t.confirm,
		Display:              tools.DefaultDisplay(t.name, t.description),
	}
}

// Execute implements tools.Tool. Records the args, returns the configured
// Result/Err (or ResultFn's output when set) wrapped in a tools.Result.
func (t *Tool) Execute(_ context.Context, argsJSON string) (tools.Result, error) {
	t.calls.Add(1)
	t.mu.Lock()
	t.lastArgs = argsJSON
	t.allArgs = append(t.allArgs, argsJSON)
	t.mu.Unlock()
	if t.ResultFn != nil {
		s, err := t.ResultFn(argsJSON)
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Text(s), nil
	}
	if t.Err != nil {
		return tools.Result{}, t.Err
	}
	return tools.Text(t.Result), nil
}

// Calls returns how many times Execute has been invoked so far.
func (t *Tool) Calls() int64 { return t.calls.Load() }

// LastArgs returns the argsJSON passed to the most recent Execute call, or
// "" if Execute was never called.
func (t *Tool) LastArgs() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastArgs
}

// AllArgs returns every argsJSON passed across every Execute call, in
// invocation order. The returned slice is a copy.
func (t *Tool) AllArgs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.allArgs))
	copy(out, t.allArgs)
	return out
}
