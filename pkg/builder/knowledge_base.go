package builder

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
// Documents with empty Path or Content are dropped by FormatKnowledgeBase.
type KBDocument struct {
	Path    string
	Content string
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
		fmt.Fprintf(&sb, "<file path=%q>\n", d.Path)
		sb.WriteString(d.Content)
		if d.Content[len(d.Content)-1] != '\n' {
			sb.WriteByte('\n')
		}
		sb.WriteString("</file>\n")
	}
	sb.WriteString("</knowledge_base>\n")
	return sb.String()
}

// LoadKnowledgeBase walks dir recursively, reads every text file whose
// extension is in kbExtensions, and returns the FormatKnowledgeBase output
// for the resulting document set. Relative paths inside the block use
// forward slashes regardless of host OS.
//
// An empty dir, or a dir with no whitelisted files, yields ("", nil) so
// callers can unconditionally append the result to a system prompt.
//
// The function deliberately has no size cap; pointing it at a 1GB directory
// will produce a 1GB prompt and break the LLM request. Trust the caller to
// point it at curated reference material.
func LoadKnowledgeBase(dir string) (string, error) {
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
		ext := strings.ToLower(filepath.Ext(path))
		if !kbExtensions[ext] {
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
		docs = append(docs, KBDocument{
			Path:    filepath.ToSlash(rel),
			Content: string(data),
		})
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
