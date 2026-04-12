// Package tools defines the Tool interface and a thread-safe Registry for managing agent tools.
package tools

import (
	"context"
	"log/slog"
	"sort"
	"sync"
)

// ToolSchema defines the JSON Schema for a tool's parameters.
// It maps directly to the OpenAI/Anthropic function calling schema format.
type ToolSchema struct {
	Type       string                 `json:"type"`                 // typically "object"
	Properties map[string]interface{} `json:"properties,omitempty"` // parameter definitions
	Required   []string               `json:"required,omitempty"`   // required parameter names
}

// Tool defines the interface for all agent tools.
type Tool interface {
	// Name returns the unique identifier for this tool.
	Name() string
	// Description returns a human-readable description of what this tool does.
	Description() string
	// ParametersSchema returns the JSON Schema describing the tool's input parameters.
	ParametersSchema() ToolSchema
	// RequiresConfirmation returns true if the tool needs human approval before execution.
	RequiresConfirmation() bool
	// Execute runs the tool with the given JSON arguments and returns the result.
	Execute(ctx context.Context, argsJSON string) (string, error)
}

// InlineRenderer is optionally implemented by tools whose output should be
// streamed directly to the frontend as content (e.g. images, videos, HTML widgets).
// The result is emitted as a "content" StreamEvent in addition to being fed back
// to the LLM as a normal tool result.
type InlineRenderer interface {
	InlineResult() bool
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
