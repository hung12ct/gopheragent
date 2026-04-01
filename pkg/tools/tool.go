// Package tools defines the Tool interface and a thread-safe Registry for managing agent tools.
package tools

import (
	"context"
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

// Registry manages the available tools for an agent. Safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool to the registry. Overwrites any existing tool with the same name.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Get retrieves a tool by name. Returns false if not found.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// GetAll returns all registered tools in deterministic alphabetical order.
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
		result = append(result, r.tools[name])
	}
	return result
}
