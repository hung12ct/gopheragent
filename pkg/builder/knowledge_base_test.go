package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKnowledgeBase_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "api.md", "# API\nUse POST /v1/chat.")
	writeFile(t, dir, "policies.txt", "No secrets in prompts.")

	out, err := LoadKnowledgeBase(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "<knowledge_base>") || !strings.Contains(out, "</knowledge_base>") {
		t.Fatal("output must be wrapped in <knowledge_base> tags")
	}
	if !strings.Contains(out, `<file path="api.md">`) {
		t.Fatalf("missing api.md file tag: %q", out)
	}
	if !strings.Contains(out, `<file path="policies.txt">`) {
		t.Fatalf("missing policies.txt file tag: %q", out)
	}
	if !strings.Contains(out, "Use POST /v1/chat") || !strings.Contains(out, "No secrets in prompts.") {
		t.Fatal("file contents not embedded")
	}
}

func TestLoadKnowledgeBase_DeterministicOrdering(t *testing.T) {
	// Same dir must produce identical bytes on every call so prompt-cache
	// breakpoints keyed on the system prompt stay hot.
	dir := t.TempDir()
	writeFile(t, dir, "b.md", "bravo")
	writeFile(t, dir, "a.md", "alpha")
	writeFile(t, dir, "c.md", "charlie")

	first, _ := LoadKnowledgeBase(dir)
	second, _ := LoadKnowledgeBase(dir)
	if first != second {
		t.Fatal("LoadKnowledgeBase must be deterministic across calls")
	}
	// Alphabetical order: a < b < c.
	idxA := strings.Index(first, "a.md")
	idxB := strings.Index(first, "b.md")
	idxC := strings.Index(first, "c.md")
	if !(idxA < idxB && idxB < idxC) {
		t.Fatalf("expected alphabetical ordering a,b,c — got offsets a=%d b=%d c=%d", idxA, idxB, idxC)
	}
}

func TestLoadKnowledgeBase_SkipsNonWhitelistedExtensions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "readme.md", "readme content")
	writeFile(t, dir, "binary.png", "\x89PNG fake")
	writeFile(t, dir, "config.yaml", "key: value")

	out, err := LoadKnowledgeBase(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "readme content") {
		t.Fatal(".md file should be included")
	}
	if strings.Contains(out, "PNG fake") || strings.Contains(out, "key: value") {
		t.Fatal(".png and .yaml should be skipped")
	}
}

func TestLoadKnowledgeBase_NestedDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "top.md", "top-level")
	writeFile(t, dir, "sub/nested.md", "nested")

	out, err := LoadKnowledgeBase(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `<file path="top.md">`) {
		t.Fatal("top-level file missing")
	}
	if !strings.Contains(out, `<file path="sub/nested.md">`) {
		t.Fatalf("expected forward-slashed nested path, got: %q", out)
	}
}

func TestLoadKnowledgeBase_EmptyDirYieldsEmptyString(t *testing.T) {
	dir := t.TempDir()
	out, err := LoadKnowledgeBase(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty output for empty dir, got %q", out)
	}
}

func TestLoadKnowledgeBase_NonExistentDirReturnsError(t *testing.T) {
	_, err := LoadKnowledgeBase("/does/not/exist/kb-xyz")
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
}

func TestLoadKnowledgeBase_FileInsteadOfDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-dir.md")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKnowledgeBase(path); err == nil {
		t.Fatal("expected error when path is a file, not a directory")
	}
}

func TestWithKnowledgeBase_EmptyDirReturnsBaseUnchanged(t *testing.T) {
	got, err := WithKnowledgeBase("base prompt", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "base prompt" {
		t.Fatalf("expected unchanged prompt, got %q", got)
	}
}

func TestWithKnowledgeBase_AppendsBlock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "faq.md", "Q: what? A: this.")

	got, err := WithKnowledgeBase("You are a helper.", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "You are a helper.") {
		t.Fatal("base prompt must come first so user instructions stay closest to the message")
	}
	if !strings.Contains(got, "<knowledge_base>") || !strings.Contains(got, "Q: what? A: this.") {
		t.Fatal("KB content not appended")
	}
}

func TestWithKnowledgeBase_EmptyBaseWithKB(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "alpha")
	got, err := WithKnowledgeBase("", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "<knowledge_base>") {
		t.Fatal("with empty base, KB block should be returned on its own")
	}
}

// --- YAML integration ---

func TestBuildFromYAML_KnowledgeBaseResolvesRelativeToYAML(t *testing.T) {
	tmp := t.TempDir()
	kbDir := filepath.Join(tmp, "kb")
	writeFile(t, kbDir, "guide.md", "Always cite sources.")
	writeFile(t, tmp, "agent.yaml", `
agent:
  name: "KB Agent"
  system_prompt: "You are concise."
  knowledge_base: "./kb"
`)

	cfg, err := ParseYAMLConfig(filepath.Join(tmp, "agent.yaml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(cfg.Agent.SystemPrompt, "You are concise.") {
		t.Fatal("base system prompt lost after KB injection")
	}
	if !strings.Contains(cfg.Agent.SystemPrompt, "Always cite sources.") {
		t.Fatalf("KB file content not merged into system_prompt: %q", cfg.Agent.SystemPrompt)
	}
	if !strings.Contains(cfg.Agent.SystemPrompt, "<knowledge_base>") {
		t.Fatal("KB wrapper tag missing from merged system_prompt")
	}
}

func TestBuildFromYAML_KnowledgeBaseMissingDirReturnsError(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, tmp, "agent.yaml", `
agent:
  name: "Broken KB"
  system_prompt: "hi"
  knowledge_base: "./does-not-exist"
`)
	_, _, _, err := BuildFromYAML(filepath.Join(tmp, "agent.yaml"), NewGlobalCatalog(), nil, nil)
	if err == nil {
		t.Fatal("expected error when knowledge_base directory is missing")
	}
}

// --- in-memory KB ---

func TestFormatKnowledgeBase_InMemoryDocs(t *testing.T) {
	out := FormatKnowledgeBase([]KBDocument{
		{Path: "faq.md", Content: "Q: what? A: this."},
		{Path: "policy.txt", Content: "No secrets."},
	})
	if !strings.Contains(out, `<file path="faq.md">`) || !strings.Contains(out, `<file path="policy.txt">`) {
		t.Fatalf("file tags missing: %q", out)
	}
	if !strings.Contains(out, "Q: what? A: this.") || !strings.Contains(out, "No secrets.") {
		t.Fatal("content not embedded")
	}
}

func TestFormatKnowledgeBase_MatchesLoadOutputForEquivalentInput(t *testing.T) {
	// Byte-identical output guarantees callers get the same prompt-cache
	// key whether the KB came from disk or memory.
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "alpha\n")
	writeFile(t, dir, "b.md", "bravo\n")

	fromDisk, err := LoadKnowledgeBase(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fromMem := FormatKnowledgeBase([]KBDocument{
		{Path: "a.md", Content: "alpha\n"},
		{Path: "b.md", Content: "bravo\n"},
	})
	if fromDisk != fromMem {
		t.Fatalf("disk and in-memory outputs differ:\n--disk--\n%s\n--mem--\n%s", fromDisk, fromMem)
	}
}

func TestFormatKnowledgeBase_DropsEmptyEntries(t *testing.T) {
	out := FormatKnowledgeBase([]KBDocument{
		{Path: "", Content: "orphan"},       // no path — drop
		{Path: "blank.md", Content: ""},     // no content — drop
		{Path: "real.md", Content: "kept"},  // keep
	})
	if !strings.Contains(out, "kept") {
		t.Fatal("valid entry dropped")
	}
	if strings.Contains(out, "orphan") || strings.Contains(out, "blank.md") {
		t.Fatalf("empty entries leaked into output: %q", out)
	}
}

func TestFormatKnowledgeBase_EmptyInputYieldsEmptyString(t *testing.T) {
	if out := FormatKnowledgeBase(nil); out != "" {
		t.Fatalf("expected empty output for nil docs, got %q", out)
	}
	if out := FormatKnowledgeBase([]KBDocument{}); out != "" {
		t.Fatalf("expected empty output for zero docs, got %q", out)
	}
}

func TestFormatKnowledgeBase_SortsByPath(t *testing.T) {
	out := FormatKnowledgeBase([]KBDocument{
		{Path: "zeta.md", Content: "z"},
		{Path: "alpha.md", Content: "a"},
		{Path: "mid.md", Content: "m"},
	})
	idxA := strings.Index(out, "alpha.md")
	idxM := strings.Index(out, "mid.md")
	idxZ := strings.Index(out, "zeta.md")
	if !(idxA < idxM && idxM < idxZ) {
		t.Fatalf("expected alphabetical ordering; offsets a=%d m=%d z=%d", idxA, idxM, idxZ)
	}
}

func TestWithKnowledgeBaseDocs_AppendsBlock(t *testing.T) {
	got := WithKnowledgeBaseDocs("You are concise.", []KBDocument{
		{Path: "guide.md", Content: "cite sources"},
	})
	if !strings.HasPrefix(got, "You are concise.") {
		t.Fatal("base prompt must come first")
	}
	if !strings.Contains(got, "cite sources") {
		t.Fatal("doc content not appended")
	}
}

func TestWithKnowledgeBaseDocs_NilDocsReturnsBaseUnchanged(t *testing.T) {
	if got := WithKnowledgeBaseDocs("base", nil); got != "base" {
		t.Fatalf("expected unchanged prompt, got %q", got)
	}
	if got := WithKnowledgeBaseDocs("base", []KBDocument{{Path: "", Content: ""}}); got != "base" {
		t.Fatalf("all-empty docs should be a no-op, got %q", got)
	}
}

func TestBuildFromYAML_NoKnowledgeBaseLeavesPromptUntouched(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, tmp, "agent.yaml", `
agent:
  name: "Plain"
  system_prompt: "Just this."
`)
	cfg, err := ParseYAMLConfig(filepath.Join(tmp, "agent.yaml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent.SystemPrompt != "Just this." {
		t.Fatalf("expected unmodified prompt, got %q", cfg.Agent.SystemPrompt)
	}
}
