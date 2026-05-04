// Package tools defines the Tool interface and a thread-safe Registry for managing agent tools.
package tools

import (
	"context"
	"log/slog"
	"maps"
	"sort"
	"sync"
)

// ToolSchema defines the JSON Schema for a tool's parameters.
// It maps directly to the OpenAI/Anthropic function calling schema format.
type ToolSchema struct {
	Type       string                 `json:"type"`                 // typically "object"
	Properties map[string]any `json:"properties,omitempty"` // parameter definitions
	Required   []string               `json:"required,omitempty"`   // required parameter names
}

// Tool defines the interface for all agent tools.
//
// Display returns UI-facing metadata (friendly label, description,
// category, icon hint) consumed by chat clients rendering tool calls.
// Tools with no special UI treatment can return
// tools.DefaultDisplay(t.Name(), t.Description()) as a one-liner.
type Tool interface {
	// Name returns the unique identifier for this tool.
	Name() string
	// Description returns a human-readable description of what this tool does.
	Description() string
	// ParametersSchema returns the JSON Schema describing the tool's input parameters.
	ParametersSchema() ToolSchema
	// RequiresConfirmation returns true if the tool needs human approval before execution.
	RequiresConfirmation() bool
	// Display returns UI-facing metadata (label, description, category,
	// icon hint). See DefaultDisplay for a one-liner fallback.
	Display() ToolDisplay
	// Execute runs the tool with the given JSON arguments and returns the result.
	Execute(ctx context.Context, argsJSON string) (string, error)
}

// InlineRenderer is optionally implemented by tools whose output should be
// streamed directly to the frontend as content (e.g. images, videos, HTML widgets).
// The result is emitted as a "content" StreamEvent in addition to being fed back
// to the LLM as a normal tool result.
//
// WRAPPER WARNING: this is a method-set interface. Wrapping a tool that
// implements InlineRenderer (e.g. embedding *GenerateImageTool inside an
// adapter struct that overrides Name() or Execute()) WILL drop the
// optional method silently — Go's interface satisfaction is structural,
// and the wrapper struct does not inherit InlineResult() unless you
// re-declare it. Symptom: the model paraphrases the markdown URL instead
// of emitting it verbatim, the FE never sees the image. If you wrap a
// tool that implements InlineRenderer, your wrapper MUST also implement it:
//
//	type MyImageWrapper struct{ *builtin.GenerateImageTool }
//	func (w *MyImageWrapper) InlineResult() bool { return w.GenerateImageTool.InlineResult() }
type InlineRenderer interface {
	InlineResult() bool
}

// Cacheable is optionally implemented by tools that produce deterministic
// results safe to cache by the agent loop. Tools that do not implement this
// interface — or return false — bypass caching entirely even when the
// AgentLoop has a Cache configured. This is explicit opt-in to avoid silent
// staleness for live-data tools (prices, weather, time, etc.).
//
// WRAPPER WARNING: same caveat as InlineRenderer — wrapping a Cacheable
// tool drops cacheability silently unless the wrapper re-declares the
// method. Symptom: every call hits the underlying provider even when the
// agent has a Cache configured. Forward explicitly:
//
//	func (w *MyToolWrapper) Cacheable() bool { return w.Tool.Cacheable() }
type Cacheable interface {
	Cacheable() bool
}

// progressKey is the unexported context key for the progress reporting function.
type progressKey struct{}

// WithProgressFunc injects a progress-reporting callback into ctx. The agent
// loop calls this before invoking Execute so that long-running tools can emit
// "tool_progress" stream events without knowing about the streaming layer.
//
// The returned context should be passed directly to tool.Execute.
func WithProgressFunc(ctx context.Context, f func(msg string)) context.Context {
	return context.WithValue(ctx, progressKey{}, f)
}

// ReportProgress emits a progress message if a reporting function was injected
// into ctx by WithProgressFunc. It is a no-op when called outside an agent loop
// (e.g. in tests that do not set up the reporting function).
//
// Call this inside Execute to emit status updates while a long-running operation
// is in progress:
//
//	tools.ReportProgress(ctx, "Polling operation (30s elapsed)…")
func ReportProgress(ctx context.Context, msg string) {
	if f, ok := ctx.Value(progressKey{}).(func(string)); ok && f != nil {
		f(msg)
	}
}

// Registry manages the available tools for an agent. Safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	tools   map[string]Tool
	wrapped map[string]Tool // cached debug-wrapped tools
	debug   bool
	logger  *slog.Logger
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// EnableDebug turns on per-call logging for every tool in this registry.
// Pass nil to use slog.Default(). Calls are idempotent — calling again swaps the logger.
//
//	reg.EnableDebug(nil)                          // use default logger
//	reg.EnableDebug(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
func (r *Registry) EnableDebug(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.debug = true
	r.logger = logger
	r.wrapped = nil
}

// Register adds a tool to the registry. Overwrites any existing tool with the same name.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
	delete(r.wrapped, t.Name())
}

// Get retrieves a tool by name. Returns false if not found.
// When debug mode is enabled the tool is transparently wrapped with WithLogging.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return nil, false
	}
	if r.debug {
		t = r.debugWrapped(name, t)
	}
	return t, true
}

// GetAll returns all registered tools in deterministic alphabetical order.
// Debug wrapping is applied when enabled.
func (r *Registry) GetAll() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]Tool, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		if r.debug {
			t = r.debugWrapped(name, t)
		}
		result = append(result, t)
	}
	return result
}

// Clone returns a shallow copy of the registry with the same tools, debug
// flag, and logger. Changes to the clone do not affect the original.
func (r *Registry) Clone() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := &Registry{
		tools:  make(map[string]Tool, len(r.tools)),
		debug:  r.debug,
		logger: r.logger,
	}
	maps.Copy(c.tools, r.tools)
	return c
}

// debugWrapped returns a cached logging-wrapped tool. Must be called under r.mu.
func (r *Registry) debugWrapped(name string, t Tool) Tool {
	if w, ok := r.wrapped[name]; ok {
		return w
	}
	w := Chain(t, WithLogging(r.logger))
	if r.wrapped == nil {
		r.wrapped = make(map[string]Tool)
	}
	r.wrapped[name] = w
	return w
}
