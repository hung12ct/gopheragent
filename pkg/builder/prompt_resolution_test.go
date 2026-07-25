package builder

import (
	"path/filepath"
	"strings"
	"testing"
)

// The two routes to a final system prompt must agree byte for byte.
//
// The documented persistent-session workflow calls ParseYAMLConfig, hands
// its SystemPrompt to a File- or MySQL-backed session manager, then builds
// the loop separately. If prompt augmentation lives in only one of those
// paths, the prompt an adopter gets depends on which session backend they
// chose — a split that is invisible until someone diffs two deployments.
func TestParseYAMLConfigMatchesBuild_SystemPrompt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "kb/api.md", "# API\nUse POST /v1/chat.")
	writeFile(t, dir, "kb/nested/policies.txt", "No secrets in prompts.")
	writeFile(t, dir, "agent.yaml", `
agent:
  name: "Prompt Parity Agent"
  system_prompt: |
    You are a helpful assistant.
  knowledge_base: "./kb"
`)
	yamlPath := filepath.Join(dir, "agent.yaml")

	parsed, err := ParseYAMLConfig(yamlPath)
	if err != nil {
		t.Fatalf("ParseYAMLConfig: %v", err)
	}
	_, _, built, err := BuildFromYAML(yamlPath, NewGlobalCatalog(), nil, nil)
	if err != nil {
		t.Fatalf("BuildFromYAML: %v", err)
	}

	if parsed.Agent.SystemPrompt != built.Agent.SystemPrompt {
		t.Fatalf("system prompts diverged between parse and build:\nParseYAMLConfig: %q\nBuildFromYAML:   %q",
			parsed.Agent.SystemPrompt, built.Agent.SystemPrompt)
	}
	// Guard against both routes agreeing on nothing.
	if parsed.Agent.SystemPrompt == "" {
		t.Fatal("expected a non-empty prompt")
	}
	if !strings.Contains(parsed.Agent.SystemPrompt, "<knowledge_base>") {
		t.Fatalf("knowledge base was not applied: %q", parsed.Agent.SystemPrompt)
	}
}

// The bytes route must agree with the file route too.
func TestParseYAMLBytesMatchesBuild_SystemPrompt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "kb/api.md", "# API\nUse POST /v1/chat.")
	doc := []byte(`
agent:
  name: "Bytes Parity Agent"
  system_prompt: "Base prompt."
  knowledge_base: "./kb"
`)

	parsed, err := ParseYAMLBytes(doc, dir)
	if err != nil {
		t.Fatalf("ParseYAMLBytes: %v", err)
	}
	_, _, built, err := BuildFromYAMLBytes(doc, dir, NewGlobalCatalog(), nil, nil)
	if err != nil {
		t.Fatalf("BuildFromYAMLBytes: %v", err)
	}
	if parsed.Agent.SystemPrompt != built.Agent.SystemPrompt {
		t.Fatalf("system prompts diverged:\nParseYAMLBytes:       %q\nBuildFromYAMLBytes:   %q",
			parsed.Agent.SystemPrompt, built.Agent.SystemPrompt)
	}
}

// Relative paths with no baseDir must fail the same way on both routes,
// rather than one erroring and the other resolving against the process cwd.
func TestPromptResolution_RelativePathWithoutBaseDirErrorsOnBothRoutes(t *testing.T) {
	doc := []byte(`
agent:
  name: "Relative Agent"
  system_prompt: "Base."
  knowledge_base: "./kb"
`)
	if _, err := ParseYAMLBytes(doc, ""); err == nil {
		t.Fatal("ParseYAMLBytes should reject a relative path with no baseDir")
	}
	if _, _, _, err := BuildFromYAMLBytes(doc, "", NewGlobalCatalog(), nil, nil); err == nil {
		t.Fatal("BuildFromYAMLBytes should reject a relative path with no baseDir")
	}
}
