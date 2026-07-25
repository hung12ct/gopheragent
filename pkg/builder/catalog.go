package builder

import (
	"fmt"
	"sort"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// GlobalCatalog acts as the unified "Marketplace" where developers register their custom Tools.
// It is safe for concurrent use.
type GlobalCatalog struct {
	mu             sync.RWMutex
	availableTools map[string]tools.Tool
	middlewares    []tools.Middleware
}

// NewGlobalCatalog initializes a fresh Tool Marketplace.
func NewGlobalCatalog() *GlobalCatalog {
	return &GlobalCatalog{
		availableTools: make(map[string]tools.Tool),
	}
}

// Register adds a custom tool to the global marketplace so the YAML parser can resolve it.
func (c *GlobalCatalog) Register(tool tools.Tool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.availableTools[tool.Descriptor().Name] = tool
}

// Use registers middleware applied to every tool the catalog hands to a built
// agent, in registration order (outermost first, per tools.Chain). Call before
// building. The common use is wrapping all tools with oteltools.Instrument so a
// YAML-built agent gets per-tool spans and latency metrics without editing each
// tool. Middleware must preserve Descriptor().Name so the registry keys stay
// stable.
func (c *GlobalCatalog) Use(mw ...tools.Middleware) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.middlewares = append(c.middlewares, mw...)
}

// wrap applies the catalog's middleware chain to tool. It returns tool unchanged
// when no middleware is registered, so the no-middleware path is zero-overhead.
func (c *GlobalCatalog) wrap(tool tools.Tool) tools.Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.middlewares) == 0 {
		return tool
	}
	return tools.Chain(tool, c.middlewares...)
}

// Get retrieves a tool by its exact string name defined in the YAML flow file.
func (c *GlobalCatalog) Get(name string) (tools.Tool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tool, exists := c.availableTools[name]
	if !exists {
		return nil, fmt.Errorf("tool '%s' not found in Global Catalog. Please ensure it is registered before loading the YAML", name)
	}
	return tool, nil
}

// ListNames returns the names of all registered tools in sorted order.
func (c *GlobalCatalog) ListNames() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := make([]string, 0, len(c.availableTools))
	for k := range c.availableTools {
		names = append(names, k)
	}
	// Map iteration is randomized. This list is the "available tools" hint
	// in tools_required validation errors, so without the sort the same bad
	// config reports its options in a different order every run.
	sort.Strings(names)
	return names
}
