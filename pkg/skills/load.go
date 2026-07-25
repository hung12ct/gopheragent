package skills

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// skillFileName is matched byte-exactly against directory entries rather
// than resolved with fs.Stat: macOS and Windows filesystems answer a stat
// for "skill.md" with the same file, which would admit a skill whose name
// the spec does not sanction.
const skillFileName = "SKILL.md"

// errOversize marks a SKILL.md that blew the size bound, so admit can
// classify the rejection with errors.Is rather than by matching words in a
// message. Classification that depends on error wording breaks silently the
// first time someone rephrases the text.
var errOversize = errors.New("skills: file exceeds size limit")

// FromFS loads every skill in fsys.
//
// A skill is a directory containing a file named exactly SKILL.md; any
// regular files beneath that directory become its resource files. Loading
// does not descend into a skill, so skills cannot nest.
//
// Trust defaults to Untrusted — pass TrustedSource for content you control.
//
// Taking an fs.FS rather than a directory path is what keeps this usable
// from a server: os.DirFS covers the on-disk case in one line, an embed.FS
// needs no filesystem at all, and a custom FS covers per-tenant skills read
// from a database. It also means fs.ValidPath rejects parent-directory and
// absolute paths at the stdlib boundary, so traversal is impossible rather
// than merely guarded against.
//
// Per-skill problems never fail the call: one malformed SKILL.md must not
// break agent startup. They are recorded in Set.Skipped, which callers
// should log once — silently dropping them makes "my skill isn't loading"
// undiagnosable. An error is returned only when ctx is cancelled.
//
// Cost is O(entries) syscalls, paid once. Call it at construction and share
// the resulting Set; never call it per request.
func FromFS(ctx context.Context, fsys fs.FS, opts ...Option) (*Set, error) {
	l := &loader{cfg: newConfig(opts), byName: make(map[string]int)}
	if fsys == nil {
		return l.finish(), nil
	}
	if err := l.walk(ctx, fsys); err != nil {
		return nil, err
	}
	return l.finish(), nil
}

// New builds a Set from skills already in memory — rows from a database, a
// response from an API, values compiled into the binary. Each is validated
// exactly as FromFS validates a parsed SKILL.md.
//
// In-memory skills carry no resource files: Files is cleared, because there
// is no fs.FS behind them for Set.File to read. A source that does have
// resources should implement fs.FS and go through FromFS instead — that is
// the case the io/fs abstraction exists to cover.
//
// Trust on each input Skill is ignored and replaced by the load option, so
// one call cannot mix vouched and unvouched content. Load them separately
// and combine with Merge when a source has both.
func New(ctx context.Context, skills []Skill, opts ...Option) (*Set, error) {
	l := &loader{cfg: newConfig(opts), byName: make(map[string]int)}
	for _, s := range skills {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("skills: new: %w", err)
		}
		s.Files = nil
		s.fsys = nil
		s.Trust = l.cfg.trust
		if s.Location == "" {
			s.Location = s.Name
		}
		// In-memory skills bypass the parser, so they must still clear the
		// same field validation a parsed SKILL.md does — otherwise New is a
		// hole around every bound the loader enforces.
		fm := frontmatter{
			Name:          s.Name,
			Description:   s.Description,
			Compatibility: s.Compatibility,
			Metadata:      s.Metadata,
		}
		if err := fm.validate(); err != nil {
			l.skip(s.Location, s.Name, SkipInvalidFrontmatter, err)
			continue
		}
		l.consider(s)
	}
	return l.finish(), nil
}

// Merge combines Sets into one, first-wins on duplicate names. The losing
// skill is recorded in Skipped with SkipDuplicateName.
//
// First-wins inverts the reference implementations, which let project-level
// skills override user-level ones. For a library that ordering is a
// one-line hijack: a skill named "deploy" in a cloned repository would
// silently replace the operator's own. List trusted sources first.
//
// The merged Set adopts the most conservative resource-read limit and the
// catalog budget of the first Set, so merging cannot exceed the prompt cost
// any single source was allowed.
func Merge(sets ...*Set) (*Set, error) {
	l := &loader{byName: make(map[string]int)}
	first := true
	for _, s := range sets {
		if s == nil {
			continue
		}
		if first {
			l.cfg = s.cfg
			first = false
		} else if s.cfg.maxResourceBytes < l.cfg.maxResourceBytes {
			l.cfg.maxResourceBytes = s.cfg.maxResourceBytes
		}
		for _, sk := range s.skills {
			l.consider(sk)
		}
		l.skipped = append(l.skipped, s.skipped...)
	}
	if first {
		l.cfg = newConfig(nil)
	}
	return l.finish(), nil
}

// loader accumulates admitted skills and rejections during a load.
//
// This is a struct rather than closures over the walk because the state it
// threads — the name index, the running catalog budget, the rejection log,
// the resolved config — is five variables that every step reads and writes.
type loader struct {
	cfg          config
	skills       []Skill
	byName       map[string]int
	skipped      []Skipped
	catalogBytes int
}

// walk visits fsys, admitting every directory that holds a SKILL.md.
func (l *loader) walk(ctx context.Context, fsys fs.FS) error {
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			l.skip(p, "", SkipUnreadable, walkErr)
			return fs.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("skills: load %q: %w", p, err)
		}
		if p != "." {
			if _, skipDir := l.cfg.skipDirs[path.Base(p)]; skipDir {
				return fs.SkipDir
			}
			if depthOf(p) > l.cfg.maxDepth {
				return fs.SkipDir
			}
		}
		entries, err := fs.ReadDir(fsys, p)
		if err != nil {
			l.skip(p, "", SkipUnreadable, err)
			return fs.SkipDir
		}
		if !hasSkillFile(entries) {
			return nil
		}
		l.admit(fsys, p, entries)
		// A skill directory is a leaf: everything below it is a resource,
		// not another skill.
		return fs.SkipDir
	})
}

// admit parses the SKILL.md at dir and records the outcome.
func (l *loader) admit(fsys fs.FS, dir string, entries []fs.DirEntry) {
	name := path.Base(dir)
	if dir == "." {
		// A SKILL.md at the FS root has no directory name to match
		// against, so the spec's name==dir rule cannot be checked.
		l.skip(dir, "", SkipNameMismatch, fmt.Errorf("skills: SKILL.md at the filesystem root has no skill directory"))
		return
	}
	// MaxSkills short-circuits before any read; MaxCatalogBytes cannot,
	// because the cost depends on the name and description inside the file.
	if len(l.skills) >= l.cfg.maxSkills {
		l.skip(dir, name, SkipBudgetExhausted, nil)
		return
	}

	body, err := l.readSkillFile(fsys, dir)
	if err != nil {
		reason := SkipUnreadable
		if errors.Is(err, errOversize) {
			reason = SkipOversize
		}
		l.skip(dir, name, reason, err)
		return
	}
	fm, docBody, lenient, err := parseSkillDoc(body, l.cfg.strict)
	if err != nil {
		l.skip(dir, name, SkipInvalidFrontmatter, err)
		return
	}
	if err := fm.validate(); err != nil {
		l.skip(dir, name, SkipInvalidFrontmatter, err)
		return
	}
	// The spec requires name == directory so the catalog can never
	// advertise a skill under a name that does not resolve.
	if fm.Name != name {
		l.skip(dir, name, SkipNameMismatch,
			fmt.Errorf("skills: name %q does not match directory %q", fm.Name, name))
		return
	}

	l.consider(Skill{
		Name:          fm.Name,
		Description:   fm.Description,
		Body:          docBody,
		Location:      dir,
		Files:         collectFiles(fsys, dir, entries, l.cfg),
		Trust:         l.cfg.trust,
		License:       fm.License,
		Compatibility: fm.Compatibility,
		Metadata:      fm.Metadata,
		AllowedTools:  strings.Fields(fm.AllowedTools),
		Lenient:       lenient,
		fsys:          fsys,
	})
}

// consider applies the name-collision and catalog-budget rules to an
// already-validated skill. Shared by FromFS, New, and Merge so all three
// enforce identical admission.
func (l *loader) consider(s Skill) {
	if _, dup := l.byName[s.Name]; dup {
		l.skip(s.Location, s.Name, SkipDuplicateName, nil)
		return
	}
	if len(l.skills) >= l.cfg.maxSkills {
		l.skip(s.Location, s.Name, SkipBudgetExhausted, nil)
		return
	}
	cost := len(skillEntry(s))
	if l.catalogBytes+cost+len(catalogOpen)+len(catalogClose) > l.cfg.maxCatalogBytes {
		l.skip(s.Location, s.Name, SkipBudgetExhausted, nil)
		return
	}
	l.catalogBytes += cost
	l.byName[s.Name] = len(l.skills)
	l.skills = append(l.skills, s)
}

// readSkillFile reads dir/SKILL.md under the size bound. fs.Stat gates the
// common case cheaply; the LimitReader is the belt for filesystems whose
// reported size cannot be trusted (an fs.FS is an interface, not a disk).
func (l *loader) readSkillFile(fsys fs.FS, dir string) (string, error) {
	p := path.Join(dir, skillFileName)
	if info, err := fs.Stat(fsys, p); err == nil && info.Size() > l.cfg.maxSkillBytes {
		return "", fmt.Errorf("skills: %s is %d bytes: %w of %d", p, info.Size(), errOversize, l.cfg.maxSkillBytes)
	}
	f, err := fsys.Open(p)
	if err != nil {
		return "", fmt.Errorf("skills: open %s: %w", p, err)
	}
	defer func() { _ = f.Close() }() // read-only; a close error cannot lose data
	data, err := io.ReadAll(io.LimitReader(f, l.cfg.maxSkillBytes+1))
	if err != nil {
		return "", fmt.Errorf("skills: read %s: %w", p, err)
	}
	if int64(len(data)) > l.cfg.maxSkillBytes {
		return "", fmt.Errorf("skills: %s: %w of %d", p, errOversize, l.cfg.maxSkillBytes)
	}
	return string(data), nil
}

// skip records a rejection.
func (l *loader) skip(location, name string, reason SkipReason, err error) {
	l.skipped = append(l.skipped, Skipped{Location: location, Name: name, Reason: reason, Err: err})
}

// finish freezes the loader into an immutable Set, sorting by name so both
// the catalog and Names are byte-stable across runs.
func (l *loader) finish() *Set {
	sort.Slice(l.skills, func(i, j int) bool { return l.skills[i].Name < l.skills[j].Name })
	byName := make(map[string]int, len(l.skills))
	hasFiles := false
	for i, s := range l.skills {
		byName[s.Name] = i
		if len(s.Files) > 0 {
			hasFiles = true
		}
	}
	sort.Slice(l.skipped, func(i, j int) bool { return l.skipped[i].Location < l.skipped[j].Location })
	set := &Set{
		cfg:      l.cfg,
		skills:   l.skills,
		byName:   byName,
		skipped:  l.skipped,
		hasFiles: hasFiles,
	}
	set.catalog = renderCatalog(l.skills)
	return set
}

// hasSkillFile reports whether entries contain a regular SKILL.md.
func hasSkillFile(entries []fs.DirEntry) bool {
	for _, e := range entries {
		if e.Name() == skillFileName && e.Type().IsRegular() {
			return true
		}
	}
	return false
}

// collectFiles gathers resource paths relative to dir, sorted, excluding
// SKILL.md itself. The result is the allowlist Set.File validates against,
// which is why it is captured eagerly: a path that was not present at load
// time can never be read, no matter what appears on disk afterwards.
func collectFiles(fsys fs.FS, dir string, entries []fs.DirEntry, cfg config) []string {
	var files []string
	var walk func(sub string, subEntries []fs.DirEntry, depth int)
	walk = func(sub string, subEntries []fs.DirEntry, depth int) {
		for _, e := range subEntries {
			if len(files) >= cfg.maxFilesPerSkill {
				return
			}
			name := e.Name()
			full := path.Join(sub, name)
			if e.IsDir() {
				if _, skip := cfg.skipDirs[name]; skip || depth >= cfg.maxDepth {
					continue
				}
				nested, err := fs.ReadDir(fsys, full)
				if err != nil {
					continue
				}
				walk(full, nested, depth+1)
				continue
			}
			if !e.Type().IsRegular() {
				continue
			}
			if sub == dir && name == skillFileName {
				continue
			}
			rel, err := relTo(dir, full)
			if err != nil {
				continue
			}
			files = append(files, rel)
		}
	}
	walk(dir, entries, 0)
	sort.Strings(files)
	return files
}

// relTo returns full expressed relative to dir, using forward slashes.
func relTo(dir, full string) (string, error) {
	prefix := dir + "/"
	if !strings.HasPrefix(full, prefix) {
		return "", fmt.Errorf("skills: %q is not under %q", full, dir)
	}
	return full[len(prefix):], nil
}

// depthOf returns how many levels below the FS root p sits.
func depthOf(p string) int {
	if p == "." || p == "" {
		return 0
	}
	return strings.Count(p, "/") + 1
}
