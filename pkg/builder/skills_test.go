package builder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/skills"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"github.com/hung12ct/gopheragent/pkg/tools/builtin"
	"gopkg.in/yaml.v3"
)

// writeSkill creates dir/<name>/SKILL.md under root.
func writeSkill(t *testing.T, root, name, desc string) {
	t.Helper()
	writeFile(t, root, filepath.Join(name, "SKILL.md"),
		fmt.Sprintf("---\nname: %s\ndescription: %s\n---\nInstructions for %s.\n", name, desc, name))
}

func TestWithSkillCatalog_AppendsAfterBasePrompt(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "Does alpha.")

	set, err := skills.FromFS(context.Background(), os.DirFS(dir), skills.TrustedSource())
	if err != nil {
		t.Fatalf("FromFS: %v", err)
	}
	got := WithSkillCatalog("Base prompt.", set)
	if !strings.HasPrefix(got, "Base prompt.\n\n") {
		t.Fatalf("catalog must append after the base prompt: %q", got)
	}
	if !strings.Contains(got, "<available_skills>") {
		t.Fatalf("catalog missing: %q", got)
	}
	// Tier one only: the body must not ride along.
	if strings.Contains(got, "Instructions for alpha.") {
		t.Fatalf("skill body leaked into the system prompt: %q", got)
	}
}

func TestWithSkillCatalog_NilAndEmptySetsLeavePromptUnchanged(t *testing.T) {
	if got := WithSkillCatalog("Base.", nil); got != "Base." {
		t.Fatalf("nil set must not alter the prompt: %q", got)
	}
	empty, err := skills.FromFS(context.Background(), os.DirFS(t.TempDir()))
	if err != nil {
		t.Fatalf("FromFS: %v", err)
	}
	if got := WithSkillCatalog("Base.", empty); got != "Base." {
		t.Fatalf("empty set must not alter the prompt: %q", got)
	}
}

func TestSkillSource_UnmarshalsScalarAndMapping(t *testing.T) {
	var cfg SkillsConfig
	doc := `
sources:
  - ./shorthand
  - dir: ./explicit
    trust: untrusted
  - dir: ./defaulted
`
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cfg.Sources) != 3 {
		t.Fatalf("want 3 sources, got %d", len(cfg.Sources))
	}
	// A path typed into the operator's own config is operator-authored.
	if cfg.Sources[0].Dir != "./shorthand" || cfg.Sources[0].Trust != trustTrusted {
		t.Fatalf("shorthand should default to trusted: %+v", cfg.Sources[0])
	}
	if cfg.Sources[1].Dir != "./explicit" || cfg.Sources[1].Trust != trustUntrusted {
		t.Fatalf("explicit untrusted not honored: %+v", cfg.Sources[1])
	}
	if cfg.Sources[2].Trust != trustTrusted {
		t.Fatalf("mapping without trust should default to trusted: %+v", cfg.Sources[2])
	}
}

func TestValidateSkillsConfig_RejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		cfg  *SkillsConfig
		want string
	}{
		{"nil is fine", nil, ""},
		{"empty sources", &SkillsConfig{}, "sources is empty"},
		{"missing dir", &SkillsConfig{Sources: []SkillSource{{Trust: trustTrusted}}}, "dir is required"},
		{"bad trust", &SkillsConfig{Sources: []SkillSource{{Dir: "./x", Trust: "maybe"}}}, "must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues := validateSkillsConfig(tt.cfg, nil)
			if tt.want == "" {
				if len(issues) != 0 {
					t.Fatalf("expected no issues, got %v", issues)
				}
				return
			}
			if len(issues) == 0 || !strings.Contains(strings.Join(issues, " "), tt.want) {
				t.Fatalf("want an issue containing %q, got %v", tt.want, issues)
			}
		})
	}
}

// The reason resolvePrompt had to become the single source of truth: an
// augmentation reachable from only one route silently varies the prompt by
// session backend.
func TestSkills_ParseAndBuildProduceIdenticalPrompt(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "Does alpha.")
	writeSkill(t, dir, "bravo", "Does bravo.")
	writeFile(t, dir, "agent.yaml", `
agent:
  name: "Skilled Agent"
  system_prompt: "Base prompt."
  skills:
    sources:
      - "./"
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
		t.Fatalf("prompts diverged:\nParse: %q\nBuild: %q", parsed.Agent.SystemPrompt, built.Agent.SystemPrompt)
	}
	if !strings.Contains(parsed.Agent.SystemPrompt, "<name>alpha</name>") {
		t.Fatalf("skill catalog missing from prompt: %q", parsed.Agent.SystemPrompt)
	}
}

func TestSkills_BuildRegistersActivationTools(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "Does alpha.")
	writeFile(t, dir, "agent.yaml", `
agent:
  name: "Skilled Agent"
  system_prompt: "Base prompt."
  skills:
    sources:
      - "./"
`)
	loop, _, _, err := BuildFromYAMLContext(context.Background(),
		filepath.Join(dir, "agent.yaml"), NewGlobalCatalog(), nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildFromYAMLContext: %v", err)
	}
	if _, ok := loop.Tools.Get(builtin.SkillToolName); !ok {
		t.Fatalf("read_skill not registered; registry has %v", toolNames(loop.Tools))
	}
}

// Skill tools must go through GlobalCatalog.Use middleware like every other
// tool, or otel spans stop at skill activations without anyone noticing.
func TestSkills_ToolsPassThroughCatalogMiddleware(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "Does alpha.")
	writeFile(t, dir, "agent.yaml", `
agent:
  name: "Skilled Agent"
  system_prompt: "Base prompt."
  skills:
    sources:
      - "./"
`)
	var calls int
	catalog := NewGlobalCatalog()
	catalog.Use(func(next tools.Tool) tools.Tool {
		return countingTool{Tool: next, calls: &calls}
	})

	loop, _, _, err := BuildFromYAMLContext(context.Background(),
		filepath.Join(dir, "agent.yaml"), catalog, nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildFromYAMLContext: %v", err)
	}
	tool, ok := loop.Tools.Get(builtin.SkillToolName)
	if !ok {
		t.Fatal("read_skill not registered")
	}
	if _, err := tool.Execute(context.Background(), `{"name":"alpha"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls != 1 {
		t.Fatalf("catalog middleware did not observe the skill activation (calls=%d)", calls)
	}
}

func TestSkills_UntrustedSourceGetsGatedTool(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "cloned", "From a repo I cloned.")
	writeFile(t, dir, "agent.yaml", `
agent:
  name: "Skilled Agent"
  system_prompt: "Base prompt."
  skills:
    sources:
      - dir: "./"
        trust: untrusted
`)
	loop, _, _, err := BuildFromYAMLContext(context.Background(),
		filepath.Join(dir, "agent.yaml"), NewGlobalCatalog(), nil, nil, nil)
	if err != nil {
		t.Fatalf("BuildFromYAMLContext: %v", err)
	}
	if _, ok := loop.Tools.Get(builtin.SkillToolName); ok {
		t.Fatal("no trusted skills exist, so the ungated tool must not be registered")
	}
	gated, ok := loop.Tools.Get(builtin.SkillUntrustedToolName)
	if !ok {
		t.Fatalf("gated tool missing; registry has %v", toolNames(loop.Tools))
	}
	if !gated.Descriptor().RequiresConfirmation {
		t.Fatal("an untrusted source must require confirmation before its instructions load")
	}
}

func TestSkills_MissingDirIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "agent.yaml", `
agent:
  name: "Skilled Agent"
  system_prompt: "Base prompt."
  skills:
    sources:
      - "./does-not-exist"
`)
	if _, err := ParseYAMLConfig(filepath.Join(dir, "agent.yaml")); err == nil {
		t.Fatal("a configured directory that does not exist should be reported, not skipped")
	}
}

func TestSkills_NoSkillsBlockRegistersNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "agent.yaml", `
agent:
  name: "Plain Agent"
  system_prompt: "Base prompt."
`)
	loop, _, cfg, err := BuildFromYAML(filepath.Join(dir, "agent.yaml"), NewGlobalCatalog(), nil, nil)
	if err != nil {
		t.Fatalf("BuildFromYAML: %v", err)
	}
	if cfg.Agent.SystemPrompt != "Base prompt." {
		t.Fatalf("prompt should be untouched: %q", cfg.Agent.SystemPrompt)
	}
	if loop.Tools.Len() != 0 {
		t.Fatalf("expected no tools, got %v", toolNames(loop.Tools))
	}
}

// countingTool records Execute calls so a test can prove middleware ran.
type countingTool struct {
	tools.Tool
	calls *int
}

func (c countingTool) Execute(ctx context.Context, argsJSON string) (tools.Result, error) {
	*c.calls++
	return c.Tool.Execute(ctx, argsJSON)
}

func toolNames(reg *tools.Registry) []string {
	out := make([]string, 0, reg.Len())
	for _, t := range reg.All() {
		out = append(out, t.Descriptor().Name)
	}
	return out
}
