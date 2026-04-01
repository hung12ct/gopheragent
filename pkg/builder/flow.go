// Package builder provides YAML-driven agent configuration and a global tool catalog.
package builder

import (
	"fmt"
	"os"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"gopkg.in/yaml.v3"
)

// AgentConfig represents the exact schema users map their YAML files to.
//
//	agent:
//	  name: "My Agent"                    # required
//	  description: "What this agent does" # optional
//	  model: "gpt-4o"                     # optional (informational)
//	  max_iterations: 15                  # optional (default: 15)
//	  emit_thoughts: true                 # optional (default: true)
//	  system_prompt: |                    # required
//	    You are a helpful assistant.
//	  tools_required:                     # optional
//	    - "web_search"
//	    - "read_url"
type AgentConfig struct {
	Agent struct {
		Name          string   `yaml:"name"`
		Description   string   `yaml:"description"`
		Model         string   `yaml:"model"`
		MaxIterations int      `yaml:"max_iterations"`
		EmitThoughts  *bool    `yaml:"emit_thoughts,omitempty"`
		SystemPrompt  string   `yaml:"system_prompt"`
		ToolsRequired []string `yaml:"tools_required"`
	} `yaml:"agent"`
}

// YAMLValidationError holds all validation issues found in the YAML config.
// Its Error() message lists every problem so the user can fix them all in one pass.
type YAMLValidationError struct {
	Path   string   // file path for context
	Issues []string // human-readable issue descriptions
}

func (e *YAMLValidationError) Error() string {
	return fmt.Sprintf("YAML validation failed for %q:\n  - %s", e.Path, strings.Join(e.Issues, "\n  - "))
}

// validateConfig checks the parsed config for common mistakes and returns
// a YAMLValidationError if any issues are found, or nil if valid.
func validateConfig(path string, config *AgentConfig, catalog *GlobalCatalog) error {
	var issues []string

	if config.Agent.Name == "" {
		issues = append(issues, "agent.name is required (give your agent a name)")
	}
	if config.Agent.SystemPrompt == "" {
		issues = append(issues, "agent.system_prompt is required (this tells the LLM how to behave)")
	}
	if config.Agent.MaxIterations < 0 {
		issues = append(issues, fmt.Sprintf("agent.max_iterations must be >= 0, got %d", config.Agent.MaxIterations))
	}
	if config.Agent.MaxIterations > 100 {
		issues = append(issues, fmt.Sprintf("agent.max_iterations=%d is unusually high (recommended: 5-20)", config.Agent.MaxIterations))
	}

	for i, toolName := range config.Agent.ToolsRequired {
		if strings.TrimSpace(toolName) == "" {
			issues = append(issues, fmt.Sprintf("agent.tools_required[%d] is empty — remove it or specify a tool name", i))
			continue
		}
		if catalog != nil {
			if _, err := catalog.Get(toolName); err != nil {
				available := catalog.ListNames()
				hint := ""
				if len(available) > 0 {
					hint = fmt.Sprintf(" (available tools: %s)", strings.Join(available, ", "))
				}
				issues = append(issues, fmt.Sprintf("agent.tools_required: %q not found in catalog%s — did you forget to register it?", toolName, hint))
			}
		}
	}

	seen := make(map[string]bool)
	for _, toolName := range config.Agent.ToolsRequired {
		if seen[toolName] {
			issues = append(issues, fmt.Sprintf("agent.tools_required: %q is listed twice — remove the duplicate", toolName))
		}
		seen[toolName] = true
	}

	if len(issues) > 0 {
		return &YAMLValidationError{Path: path, Issues: issues}
	}
	return nil
}

// BuildFromYAML reads a YAML config file, validates it, resolves tools from the
// catalog, and returns a fully wired AgentLoop ready to run.
//
// Returns clear, actionable error messages when the YAML has issues — designed
// so non-engineers (PMs, ops) can fix problems without reading Go code.
//
// llm: the LLM provider to use. If nil, falls back to MockProvider (useful for tests).
// hook: optional before-hook middleware. Pass nil if not needed.
//
// Session manager defaults to InMemSessionManager.
// For persistent sessions (survives restarts), use BuildFromYAMLWithSession and
// pass a MySQLSessionManager or FileSessionManager.
func BuildFromYAML(yamlPath string, catalog *GlobalCatalog, llm agent.LLMProvider, hook agent.Hook) (*agent.AgentLoop, *history.InMemSessionManager, AgentConfig, error) {
	loop, sm, cfg, err := buildFromYAML(yamlPath, catalog, llm, hook, nil)
	if err != nil {
		return nil, nil, cfg, err
	}
	inMem, _ := sm.(*history.InMemSessionManager)
	return loop, inMem, cfg, nil
}

// BuildFromYAMLWithSession is like BuildFromYAML but accepts any SessionManager —
// use this for production deployments where sessions must survive server restarts:
//
//	// MySQL-backed (persistent):
//	sm, _ := history.NewMySQLSessionManager(db, config.Agent.SystemPrompt)
//	loop, _, cfg, err := builder.BuildFromYAMLWithSession("agent.yaml", catalog, provider, nil, sm)
//
//	// File-backed (persistent, no DB required):
//	sm, _ := history.NewFileSessionManager("./sessions", config.Agent.SystemPrompt)
//	loop, _, cfg, err := builder.BuildFromYAMLWithSession("agent.yaml", catalog, provider, nil, sm)
//
// Pass nil to fall back to the default InMemSessionManager.
func BuildFromYAMLWithSession(yamlPath string, catalog *GlobalCatalog, llm agent.LLMProvider, hook agent.Hook, sm agent.SessionManager) (*agent.AgentLoop, agent.SessionManager, AgentConfig, error) {
	return buildFromYAML(yamlPath, catalog, llm, hook, sm)
}

// ParseYAMLConfig reads and validates a YAML config file without building the agent.
// Useful when you need the system prompt before constructing the session manager.
func ParseYAMLConfig(yamlPath string) (AgentConfig, error) {
	var config AgentConfig
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return config, fmt.Errorf("cannot read %q: %w", yamlPath, err)
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("invalid YAML syntax in %q: %w", yamlPath, err)
	}
	return config, nil
}

// buildFromYAML is the shared implementation.
func buildFromYAML(yamlPath string, catalog *GlobalCatalog, llm agent.LLMProvider, hook agent.Hook, sm agent.SessionManager) (*agent.AgentLoop, agent.SessionManager, AgentConfig, error) {
	var config AgentConfig

	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, nil, config, fmt.Errorf("cannot read %q: %w — check the file path", yamlPath, err)
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, nil, config, fmt.Errorf("invalid YAML syntax in %q: %w — check indentation and formatting", yamlPath, err)
	}

	if err := validateConfig(yamlPath, &config, catalog); err != nil {
		return nil, nil, config, err
	}

	registry := tools.NewRegistry()
	for _, toolName := range config.Agent.ToolsRequired {
		tool, _ := catalog.Get(toolName) // already validated above
		registry.Register(tool)
	}

	if llm == nil {
		llm = agent.NewMockProvider()
	}
	if sm == nil {
		sm = history.NewInMemSessionManager(config.Agent.SystemPrompt)
	}

	var loop *agent.AgentLoop
	if hook != nil {
		loop = agent.NewAgentLoop(sm, registry, llm, hook)
	} else {
		loop = agent.NewAgentLoop(sm, registry, llm)
	}

	if config.Agent.MaxIterations > 0 {
		loop.MaxIters = config.Agent.MaxIterations
	}
	if config.Agent.EmitThoughts != nil {
		loop.EmitThoughts = *config.Agent.EmitThoughts
	}

	return loop, sm, config, nil
}
