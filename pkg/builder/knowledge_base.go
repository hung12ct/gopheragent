package builder

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// kbExtensions is the whitelist of file extensions LoadKnowledgeBase will
// read. Anything else in the directory is ignored — we refuse to guess
// whether an unknown binary is safe to concatenate into a prompt.
var kbExtensions = map[string]bool{
	".md":       true,
	".markdown": true,
	".txt":      true,
	".rst":      true,
}

// KBDocument is a single piece of reference material injected into the
// system prompt. Path is the label shown in <file path="…"> and also
// controls sort order; it does not need to be a real filesystem path.
// Content is the raw text, rendered verbatim.
//
// Type, Title, and Tags carry OKF (Open Knowledge Format) frontmatter when
// the source file declared any — LoadKnowledgeBase strips the YAML block
// from Content and surfaces it here so the metadata reaches the model as
// <file> attributes instead of polluting the prompt body. They are empty
// for plain (non-OKF) documents.
//
// Documents with empty Path or Content are dropped by FormatKnowledgeBase.
type KBDocument struct {
	Path    string
	Content string
	Type    string
	Title   string
	Tags    []string
}

// kbFrontmatter is the subset of OKF YAML frontmatter LoadKnowledgeBase
// reads. Unknown keys are ignored — OKF lets producers add arbitrary fields
// and asks consumers not to reject documents over unrecognised ones.
type kbFrontmatter struct {
	Type  string   `yaml:"type"`
	Title string   `yaml:"title"`
	Tags  []string `yaml:"tags"`
}

// FormatKnowledgeBase returns docs wrapped in the standard
// <knowledge_base>…</knowledge_base> block with one <file path="…">…</file>
// entry per document, sorted by Path so the output is stable across runs —
// important when downstream consumers hash the system prompt for
// prompt-cache breakpoints. Returns "" when no document carries usable
// content.
func FormatKnowledgeBase(docs []KBDocument) string {
	// Filter empties so callers can freely concat from heterogenous
	// sources (DB rows, optional uploads) without pre-cleaning.
	filtered := make([]KBDocument, 0, len(docs))
	for _, d := range docs {
		if d.Path == "" || d.Content == "" {
			continue
		}
		filtered = append(filtered, d)
	}
	if len(filtered) == 0 {
		return ""
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Path < filtered[j].Path })

	var sb strings.Builder
	sb.WriteString("<knowledge_base>\n")
	for _, d := range filtered {
		sb.WriteString(kbOpenTag(d))
		sb.WriteByte('\n')
		sb.WriteString(d.Content)
		if d.Content[len(d.Content)-1] != '\n' {
			sb.WriteByte('\n')
		}
		sb.WriteString("</file>\n")
	}
	sb.WriteString("</knowledge_base>\n")
	return sb.String()
}

// kbOpenTag renders the opening <file …> tag, adding OKF metadata as
// attributes when present so the model sees a concept's type/title/tags
// without the raw YAML frontmatter in the body. Attribute order is fixed so
// output stays byte-stable for prompt-cache breakpoints.
func kbOpenTag(d KBDocument) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<file path=%q", d.Path)
	if d.Type != "" {
		fmt.Fprintf(&sb, " type=%q", d.Type)
	}
	if d.Title != "" {
		fmt.Fprintf(&sb, " title=%q", d.Title)
	}
	if len(d.Tags) > 0 {
		fmt.Fprintf(&sb, " tags=%q", strings.Join(d.Tags, ","))
	}
	sb.WriteByte('>')
	return sb.String()
}

// KBFilter narrows which OKF concepts LoadKnowledgeBaseFiltered injects. A
// zero KBFilter matches every document — the behaviour LoadKnowledgeBase
// relies on. When Types is non-empty a document's frontmatter `type` must be
// one of them; when Tags is non-empty the document must carry at least one
// of the listed tags. The two conditions combine with AND, letting an agent
// pull only the concepts it needs instead of the whole tree.
type KBFilter struct {
	Types []string
	Tags  []string
}

// matches reports whether d satisfies the filter. Plain documents (no
// frontmatter) have an empty Type and nil Tags, so any non-empty filter
// excludes them — filtering is opt-in for OKF-annotated bundles.
func (f KBFilter) matches(d KBDocument) bool {
	if len(f.Types) > 0 && !slices.Contains(f.Types, d.Type) {
		return false
	}
	if len(f.Tags) > 0 {
		hit := false
		for _, t := range d.Tags {
			if slices.Contains(f.Tags, t) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// parseFrontmatter splits leading OKF YAML frontmatter from a markdown body.
// Frontmatter is recognised only when the file opens with a "---" fence line
// and has a matching closing "---" line, and the block between parses as a
// YAML mapping. On any deviation (no fence, malformed YAML) the original
// content is returned verbatim with zero-value metadata, so plain Markdown
// — including documents that merely use "---" as a horizontal rule — passes
// through untouched.
func parseFrontmatter(content string) (kbFrontmatter, string) {
	var meta kbFrontmatter
	s := strings.TrimPrefix(content, "\ufeff") // tolerate a UTF-8 BOM
	if !strings.HasPrefix(s, "---") {
		return meta, content
	}
	lines := strings.Split(s, "\n")
	if strings.TrimRight(lines[0], "\r") != "---" {
		return meta, content
	}
	closing := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			closing = i
			break
		}
	}
	if closing < 0 {
		return meta, content
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:closing], "\n")), &meta); err != nil {
		return kbFrontmatter{}, content
	}
	return meta, strings.TrimLeft(strings.Join(lines[closing+1:], "\n"), "\r\n")
}

// LoadKnowledgeBase walks dir recursively, reads every text file whose
// extension is in kbExtensions, and returns the FormatKnowledgeBase output
// for the resulting document set. Relative paths inside the block use
// forward slashes regardless of host OS. It is LoadKnowledgeBaseFiltered
// with an empty (match-all) filter.
//
// An empty dir, or a dir with no whitelisted files, yields ("", nil) so
// callers can unconditionally append the result to a system prompt.
//
// The function deliberately has no size cap; pointing it at a 1GB directory
// will produce a 1GB prompt and break the LLM request. Trust the caller to
// point it at curated reference material.
func LoadKnowledgeBase(dir string) (string, error) {
	return LoadKnowledgeBaseFiltered(dir, KBFilter{})
}

// LoadKnowledgeBaseFiltered is LoadKnowledgeBase restricted to the OKF
// concepts matching filter. It parses YAML frontmatter from each file,
// strips it from the injected body, and keeps only documents the filter
// admits. A zero filter loads everything.
func LoadKnowledgeBaseFiltered(dir string, filter KBFilter) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("builder: knowledge base %q: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("builder: knowledge base %q is not a directory", dir)
	}

	var docs []KBDocument
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if !kbExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("builder: read %q: %w", path, readErr)
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}
		meta, body := parseFrontmatter(string(data))
		doc := KBDocument{
			Path:    filepath.ToSlash(rel),
			Content: body,
			Type:    meta.Type,
			Title:   meta.Title,
			Tags:    meta.Tags,
		}
		if filter.matches(doc) {
			docs = append(docs, doc)
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	return FormatKnowledgeBase(docs), nil
}

// WithKnowledgeBase returns basePrompt with the KB block loaded from dir
// appended after a blank line. The block is appended (not prepended) so
// task-specific instructions in basePrompt stay closest to the user
// message — LLMs weight late-prompt tokens more heavily.
//
// When dir is empty or contains no whitelisted files, basePrompt is
// returned unchanged.
func WithKnowledgeBase(basePrompt, dir string) (string, error) {
	if dir == "" {
		return basePrompt, nil
	}
	kb, err := LoadKnowledgeBase(dir)
	if err != nil {
		return "", err
	}
	return joinPromptKB(basePrompt, kb), nil
}

// WithKnowledgeBaseFiltered is WithKnowledgeBase with an OKF metadata filter
// applied before injection — load only the concept types/tags this agent
// needs instead of the whole tree, keeping the system prompt within budget.
// A zero filter behaves exactly like WithKnowledgeBase.
func WithKnowledgeBaseFiltered(basePrompt, dir string, filter KBFilter) (string, error) {
	if dir == "" {
		return basePrompt, nil
	}
	kb, err := LoadKnowledgeBaseFiltered(dir, filter)
	if err != nil {
		return "", err
	}
	return joinPromptKB(basePrompt, kb), nil
}

// WithKnowledgeBaseDocs is the in-memory analog of WithKnowledgeBase:
// useful when KB content comes from a database, an HTTP upload, or any
// runtime source rather than a directory on disk.
//
// When docs is empty or every entry is blank, basePrompt is returned
// unchanged.
func WithKnowledgeBaseDocs(basePrompt string, docs []KBDocument) string {
	return joinPromptKB(basePrompt, FormatKnowledgeBase(docs))
}

// joinPromptKB is the shared "append KB block if non-empty" rule so the
// file-backed and in-memory helpers produce byte-identical output for
// equivalent inputs.
func joinPromptKB(basePrompt, kb string) string {
	if kb == "" {
		return basePrompt
	}
	if basePrompt == "" {
		return kb
	}
	return basePrompt + "\n\n" + kb
}
