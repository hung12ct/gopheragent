package skills

import (
	"context"
	"fmt"
	"html"
	"io"
	"path"
	"sort"
	"strings"
)

// Catalog wrapper tags. Split out so the admission budget in loader.consider
// can account for them without rendering the whole block first.
const (
	catalogOpen  = "<available_skills>\n"
	catalogClose = "</available_skills>\n"
)

// Set is the immutable result of loading skills.
//
// Every method is safe for concurrent use with no synchronization, because
// nothing mutates after construction. That is the reason skill bodies are
// read eagerly rather than on demand: an immutable Set needs no locks, costs
// no syscalls when a skill is activated in forty sessions at once, and
// leaves no window between a trust decision and the read it guards. The
// memory that buys is bounded by MaxSkills times MaxSkillBytes — 8 MiB at
// the defaults, around 1 MiB in practice.
//
// Resource files stay lazy: their fan-out is unbounded and most are never
// read. Their paths are captured eagerly, and that list is the allowlist
// File validates against.
//
// A nil *Set is usable: every method reports empty. Callers can pass the
// result of a load that found nothing without nil-checking.
type Set struct {
	cfg      config
	skills   []Skill
	byName   map[string]int
	skipped  []Skipped
	catalog  string
	hasFiles bool
}

// Len returns the number of admitted skills.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.skills)
}

// Names returns the admitted skill names, sorted. The slice is freshly
// allocated on each call: it becomes the activation tool's parameter enum,
// and a caller mutating it must not be able to poison the Set.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.skills))
	for i, sk := range s.skills {
		out[i] = sk.Name
	}
	return out
}

// NamesByTrust returns the admitted names split by trust, each sorted.
//
// The activation tools use this to build two disjoint parameter enums, so
// an untrusted skill cannot be reached through the tool that carries no
// confirmation gate.
func (s *Set) NamesByTrust() (trusted, untrusted []string) {
	if s == nil {
		return nil, nil
	}
	for _, sk := range s.skills {
		if sk.Trust == Trusted {
			trusted = append(trusted, sk.Name)
		} else {
			untrusted = append(untrusted, sk.Name)
		}
	}
	return trusted, untrusted
}

// Get returns the admitted skill by name.
func (s *Set) Get(name string) (Skill, bool) {
	if s == nil {
		return Skill{}, false
	}
	i, ok := s.byName[name]
	if !ok {
		return Skill{}, false
	}
	return s.skills[i], true
}

// HasFiles reports whether any admitted skill exposes resource files.
// Registration uses this to skip the resource-reading tool entirely when it
// would have nothing to serve.
func (s *Set) HasFiles() bool {
	if s == nil {
		return false
	}
	return s.hasFiles
}

// Skipped returns the rejected candidates, sorted by location. Log these
// once at startup: loading deliberately tolerates a malformed skill rather
// than failing, and that trade only works if the rejections are visible.
func (s *Set) Skipped() []Skipped {
	if s == nil {
		return nil
	}
	out := make([]Skipped, len(s.skipped))
	copy(out, s.skipped)
	return out
}

// Catalog renders the block that goes into the system prompt: one entry per
// skill carrying only its name, description, and location.
//
// This is tier one of progressive disclosure. It is paid on every request,
// so it holds no instructions — the model reads a description, decides, and
// spends the body's tokens only for the skill it actually uses.
//
// The output is byte-stable across runs, which lets it sit inside a
// prompt-cache prefix. Returns "" for an empty Set so callers can append it
// unconditionally.
func (s *Set) Catalog() string {
	if s == nil {
		return ""
	}
	return s.catalog
}

// Body returns the skill's SKILL.md with frontmatter stripped — tier two.
//
// Body does not gate on Trust. Trust is enforced where the model reaches
// it: the activation tools build disjoint enums from NamesByTrust and put
// the untrusted one behind a confirmation prompt. A caller holding a *Set
// already has the content in memory, so a check here would guard nothing.
//
// The error names the valid alternatives, so a hallucinated skill name
// becomes a recoverable turn rather than a dead end.
func (s *Set) Body(name string) (string, error) {
	sk, ok := s.Get(name)
	if !ok {
		return "", fmt.Errorf("skills: unknown skill %q; available: %s", name, strings.Join(s.Names(), ", "))
	}
	return sk.Body, nil
}

// File reads one of a skill's resource files — tier three, the level that
// keeps a large reference out of the prompt until it is asked for.
//
// rel is checked by exact lookup against the allowlist captured at load
// time, not by path arithmetic. A path that was not present when the Set
// was built is rejected before any syscall, which closes traversal,
// symlink-swap, and case-folding in one rule instead of three defenses.
//
// Content over the resource limit is truncated rather than refused, so an
// oversized reference is still partly useful; truncated reports it so the
// caller can tell the model what it is missing.
func (s *Set) File(ctx context.Context, name, rel string) (content string, truncated bool, err error) {
	sk, ok := s.Get(name)
	if !ok {
		return "", false, fmt.Errorf("skills: unknown skill %q; available: %s", name, strings.Join(s.Names(), ", "))
	}
	if err := ctx.Err(); err != nil {
		return "", false, fmt.Errorf("skills: read %s/%s: %w", name, rel, err)
	}
	if sk.fsys == nil {
		return "", false, fmt.Errorf("skills: %s has no resource files", name)
	}
	if i := sort.SearchStrings(sk.Files, rel); i >= len(sk.Files) || sk.Files[i] != rel {
		return "", false, fmt.Errorf("skills: %s has no file %q; available: %s", name, rel, strings.Join(sk.Files, ", "))
	}

	f, err := sk.fsys.Open(path.Join(sk.Location, rel))
	if err != nil {
		return "", false, fmt.Errorf("skills: open %s/%s: %w", name, rel, err)
	}
	defer func() { _ = f.Close() }() // read-only; a close error cannot lose data
	limit := s.cfg.maxResourceBytes
	if limit <= 0 {
		limit = DefaultMaxResourceBytes
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", false, fmt.Errorf("skills: read %s/%s: %w", name, rel, err)
	}
	if int64(len(data)) > limit {
		return string(data[:limit]), true, nil
	}
	return string(data), false, nil
}

// renderCatalog builds the <available_skills> block once at load time so
// Catalog is a field read on the request path.
func renderCatalog(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(catalogOpen)
	for _, s := range skills {
		b.WriteString(skillEntry(s))
	}
	b.WriteString(catalogClose)
	return b.String()
}

// skillEntry renders one catalog entry. Values are HTML-escaped: a
// description is free text that may contain angle brackets, and an
// unescaped one would let a skill forge catalog structure around itself.
//
// Location is the path within the source fs.FS, not an absolute host path.
// This departs from the spec's example on purpose — an absolute path leaks
// deployment layout into the prompt and makes the catalog differ between
// machines, which breaks both prompt caching and reproducible evals.
func skillEntry(s Skill) string {
	var b strings.Builder
	b.WriteString("<skill>\n<name>")
	b.WriteString(html.EscapeString(s.Name))
	b.WriteString("</name>\n<description>")
	b.WriteString(html.EscapeString(s.Description))
	b.WriteString("</description>\n<location>")
	b.WriteString(html.EscapeString(s.Location))
	b.WriteString("</location>\n</skill>\n")
	return b.String()
}
