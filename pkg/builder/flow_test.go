package builder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

type dummyTool struct{ name string }

func (d *dummyTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        d.name,
		Description: "test",
		Display:     tools.DefaultDisplay(d.name, "test"),
	}
}

func (d *dummyTool) Execute(_ context.Context, _ string) (tools.Result, error) {
	return tools.Result{}, nil
}

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildFromYAML_ValidConfig(t *testing.T) {
	catalog := NewGlobalCatalog()
	catalog.Register(&dummyTool{name: "web_search"})

	p := writeYAML(t, `
agent:
  name: "Test Agent"
  system_prompt: "You are helpful."
  tools_required:
    - "web_search"
`)

	loop, sm, cfg, err := BuildFromYAML(p, catalog, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loop == nil || sm == nil {
		t.Fatal("expected non-nil loop and session manager")
	}
	if cfg.Agent.Name != "Test Agent" {
		t.Errorf("expected name 'Test Agent', got %q", cfg.Agent.Name)
	}
}

func TestBuildFromYAML_MissingName(t *testing.T) {
	p := writeYAML(t, `
agent:
  system_prompt: "hello"
`)
	_, _, _, err := BuildFromYAML(p, NewGlobalCatalog(), nil, nil)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	var ve *YAMLValidationError
	if !containsValidationError(err, &ve) {
		t.Fatalf("expected YAMLValidationError, got %T: %v", err, err)
	}
	if !hasIssue(ve, "agent.name") {
		t.Errorf("expected issue about agent.name, got: %v", ve.Issues)
	}
}

func TestBuildFromYAML_MissingSystemPrompt(t *testing.T) {
	p := writeYAML(t, `
agent:
  name: "Test"
`)
	_, _, _, err := BuildFromYAML(p, NewGlobalCatalog(), nil, nil)
	if err == nil {
		t.Fatal("expected error for missing system_prompt")
	}
	var ve *YAMLValidationError
	if !containsValidationError(err, &ve) {
		t.Fatalf("expected YAMLValidationError, got %T", err)
	}
	if !hasIssue(ve, "system_prompt") {
		t.Errorf("expected issue about system_prompt, got: %v", ve.Issues)
	}
}

func TestBuildFromYAML_UnknownTool(t *testing.T) {
	catalog := NewGlobalCatalog()
	catalog.Register(&dummyTool{name: "web_search"})

	p := writeYAML(t, `
agent:
  name: "Test"
  system_prompt: "hello"
  tools_required:
    - "nonexistent_tool"
`)
	_, _, _, err := BuildFromYAML(p, catalog, nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	msg := err.Error()
	if !strings.Contains(msg, "nonexistent_tool") {
		t.Errorf("expected error to mention tool name, got: %s", msg)
	}
	if !strings.Contains(msg, "web_search") {
		t.Errorf("expected error to list available tools, got: %s", msg)
	}
}

func TestBuildFromYAML_DuplicateTool(t *testing.T) {
	catalog := NewGlobalCatalog()
	catalog.Register(&dummyTool{name: "web_search"})

	p := writeYAML(t, `
agent:
  name: "Test"
  system_prompt: "hello"
  tools_required:
    - "web_search"
    - "web_search"
`)
	_, _, _, err := BuildFromYAML(p, catalog, nil, nil)
	if err == nil {
		t.Fatal("expected error for duplicate tool")
	}
	if !strings.Contains(err.Error(), "listed twice") {
		t.Errorf("expected 'listed twice' message, got: %s", err.Error())
	}
}

func TestBuildFromYAML_EmptyToolName(t *testing.T) {
	p := writeYAML(t, `
agent:
  name: "Test"
  system_prompt: "hello"
  tools_required:
    - ""
`)
	_, _, _, err := BuildFromYAML(p, NewGlobalCatalog(), nil, nil)
	if err == nil {
		t.Fatal("expected error for empty tool name")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected 'empty' in error, got: %s", err.Error())
	}
}

func TestBuildFromYAML_InvalidYAML(t *testing.T) {
	p := writeYAML(t, `{{{invalid yaml`)
	_, _, _, err := BuildFromYAML(p, NewGlobalCatalog(), nil, nil)
	if err == nil {
		t.Fatal("expected error for bad YAML")
	}
	if !strings.Contains(err.Error(), "YAML syntax") {
		t.Errorf("expected syntax error message, got: %s", err.Error())
	}
}

func TestBuildFromYAML_FileNotFound(t *testing.T) {
	_, _, _, err := BuildFromYAML("/tmp/does_not_exist_12345.yaml", NewGlobalCatalog(), nil, nil)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "cannot read") {
		t.Errorf("expected 'cannot read' message, got: %s", err.Error())
	}
}

func TestBuildFromYAML_HighMaxIterations(t *testing.T) {
	p := writeYAML(t, `
agent:
  name: "Test"
  system_prompt: "hello"
  max_iterations: 999
`)
	_, _, _, err := BuildFromYAML(p, NewGlobalCatalog(), nil, nil)
	if err == nil {
		t.Fatal("expected warning for high max_iterations")
	}
	if !strings.Contains(err.Error(), "unusually high") {
		t.Errorf("expected 'unusually high' warning, got: %s", err.Error())
	}
}

func TestBuildFromYAML_MultipleIssues(t *testing.T) {
	p := writeYAML(t, `
agent:
  max_iterations: 999
`)
	_, _, _, err := BuildFromYAML(p, NewGlobalCatalog(), nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var ve *YAMLValidationError
	if !containsValidationError(err, &ve) {
		t.Fatalf("expected YAMLValidationError, got %T", err)
	}
	if len(ve.Issues) < 3 {
		t.Errorf("expected at least 3 issues (name, prompt, iterations), got %d: %v", len(ve.Issues), ve.Issues)
	}
}

func TestValidationError_Message(t *testing.T) {
	e := &YAMLValidationError{
		Path:   "agent.yaml",
		Issues: []string{"issue one", "issue two"},
	}
	msg := e.Error()
	if !strings.Contains(msg, "agent.yaml") || !strings.Contains(msg, "issue one") || !strings.Contains(msg, "issue two") {
		t.Errorf("error message missing expected parts: %s", msg)
	}
}

// helpers

func containsValidationError(err error, target **YAMLValidationError) bool {
	if ve, ok := err.(*YAMLValidationError); ok {
		*target = ve
		return true
	}
	return false
}

func hasIssue(ve *YAMLValidationError, keyword string) bool {
	for _, issue := range ve.Issues {
		if strings.Contains(issue, keyword) {
			return true
		}
	}
	return false
}

func TestBuildFromYAMLBytes_ValidConfig(t *testing.T) {
	catalog := NewGlobalCatalog()
	catalog.Register(&dummyTool{name: "web_search"})

	data := []byte(`
agent:
  name: "Bytes Agent"
  system_prompt: "You are helpful."
  tools_required:
    - "web_search"
`)

	loop, sm, cfg, err := BuildFromYAMLBytes(data, "", catalog, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loop == nil || sm == nil {
		t.Fatal("expected non-nil loop and session manager")
	}
	if cfg.Agent.Name != "Bytes Agent" {
		t.Errorf("expected name 'Bytes Agent', got %q", cfg.Agent.Name)
	}
}

func TestBuildFromYAMLBytes_InvalidSyntax(t *testing.T) {
	_, _, _, err := BuildFromYAMLBytes([]byte("not: valid: yaml: ["), "", NewGlobalCatalog(), nil, nil)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
	if !strings.Contains(err.Error(), "invalid YAML syntax in embedded bytes") {
		t.Errorf("expected embedded-bytes error message, got: %v", err)
	}
}

func TestBuildFromConfig_ValidConfig(t *testing.T) {
	catalog := NewGlobalCatalog()
	catalog.Register(&dummyTool{name: "web_search"})

	var cfg AgentConfig
	cfg.Agent.Name = "Config Agent"
	cfg.Agent.SystemPrompt = "You are helpful."
	cfg.Agent.ToolsRequired = []string{"web_search"}

	loop, sm, got, err := BuildFromConfig(cfg, "", catalog, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loop == nil || sm == nil {
		t.Fatal("expected non-nil loop and session manager")
	}
	if got.Agent.Name != "Config Agent" {
		t.Errorf("expected name 'Config Agent', got %q", got.Agent.Name)
	}
}

func TestParseYAMLBytes_ValidConfig(t *testing.T) {
	data := []byte(`
agent:
  name: "Bytes Parser"
  system_prompt: "hello"
`)
	cfg, err := ParseYAMLBytes(data, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent.Name != "Bytes Parser" {
		t.Errorf("expected name 'Bytes Parser', got %q", cfg.Agent.Name)
	}
}

func TestParseYAMLBytes_InvalidSyntax(t *testing.T) {
	_, err := ParseYAMLBytes([]byte("not: valid: yaml: ["), "")
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
	if !strings.Contains(err.Error(), "invalid YAML syntax in embedded bytes") {
		t.Errorf("expected embedded-bytes error, got: %v", err)
	}
}

func TestParseYAMLBytes_RelativeKBWithoutBaseDir(t *testing.T) {
	data := []byte(`
agent:
  name: "KB Agent"
  system_prompt: "hi"
  knowledge_base: "./kb"
`)
	_, err := ParseYAMLBytes(data, "")
	if err == nil {
		t.Fatal("expected error when relative knowledge_base has no baseDir")
	}
	if !strings.Contains(err.Error(), "baseDir") {
		t.Errorf("expected baseDir hint, got: %v", err)
	}
}

func TestBuildFromConfig_RelativeKBWithoutBaseDir(t *testing.T) {
	var cfg AgentConfig
	cfg.Agent.Name = "KB Agent"
	cfg.Agent.SystemPrompt = "You are helpful."
	cfg.Agent.KnowledgeBase = "./kb"

	_, _, _, err := BuildFromConfig(cfg, "", NewGlobalCatalog(), nil, nil)
	if err == nil {
		t.Fatal("expected error when relative knowledge_base has no baseDir")
	}
	if !strings.Contains(err.Error(), "baseDir") {
		t.Errorf("expected baseDir hint in error, got: %v", err)
	}
}
