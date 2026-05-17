package builder

import (
	"fmt"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// GlobalCatalog acts as the unified "Marketplace" where developers register their custom Tools.
// It is safe for concurrent use.
type GlobalCatalog struct {
	mu             sync.RWMutex
	availableTools map[string]tools.Tool
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
	return names
}
