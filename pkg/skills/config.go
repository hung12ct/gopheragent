package skills

import "fmt"

// Default bounds. Every one is overridable with the matching Option.
//
// The catalog sits in the system prompt on every request, and skill bodies
// are held resident for the process lifetime, so both need a ceiling that
// holds even when the source tree is larger than anyone intended. The
// figures below are sized for the steady state (5-30 curated skills at
// ~8 KiB each) with headroom, not for the worst case being routine.
const (
	// DefaultMaxSkills caps admitted skills. At the default catalog budget
	// this is rarely the binding constraint; it exists so a runaway tree
	// stops at a known count rather than at whatever fits in bytes.
	DefaultMaxSkills = 128
	// DefaultMaxSkillBytes caps one SKILL.md. The spec recommends bodies
	// under ~5k tokens; 64 KiB (~16k tokens) allows well past that while
	// still bounding a single pathological file.
	DefaultMaxSkillBytes int64 = 64 << 10
	// DefaultMaxCatalogBytes caps the rendered catalog, and is the bound
	// that matters most: the catalog is paid on EVERY request, inside the
	// prompt-cache prefix. 32 KiB is ~8k tokens worst case, ~1k typical.
	//
	// Without this bound, MaxSkills skills each carrying a maximum-length
	// name and description would render ~182 KB — roughly 45k prompt
	// tokens per call.
	DefaultMaxCatalogBytes = 32 << 10
	// DefaultMaxResourceBytes caps one Set.File read. Larger files are
	// truncated, not rejected, so a big reference is still partly useful.
	DefaultMaxResourceBytes int64 = 256 << 10
	// DefaultMaxFilesPerSkill caps captured resource paths per skill.
	DefaultMaxFilesPerSkill = 256
	// DefaultMaxDepth caps directory descent. Deep enough for skills nested
	// a few groups down, shallow enough that a generated or symlinked tree
	// cannot be walked forever.
	//
	// It applies independently to the two descents: finding skill
	// directories below the FS root, and collecting resource files below a
	// skill. The budget deliberately restarts at each skill, so a skill
	// nested near the limit still gets its references/ subtree instead of
	// silently exposing no files. Worst-case absolute depth is therefore
	// 2 x MaxDepth; MaxFilesPerSkill bounds the result either way.
	DefaultMaxDepth = 8

	// MaxNameLen and MaxDescriptionLen come from the Agent Skills spec.
	MaxNameLen        = 64
	MaxDescriptionLen = 1024
	// MaxCompatibilityLen also comes from the spec.
	MaxCompatibilityLen = 500
	// MaxMetadataKeys and MaxMetadataValueLen do not: the spec leaves
	// metadata unbounded, which is an accumulator with no ceiling. These
	// are this package's own limits.
	MaxMetadataKeys     = 32
	MaxMetadataValueLen = 256
)

// defaultSkipDirs are never descended into. They are the directories that
// cost the most to walk and can never contain a curated skill.
var defaultSkipDirs = []string{
	".git", "node_modules", "vendor", ".venv", "venv",
	"__pycache__", ".idea", ".vscode", "dist", "build", "target",
}

// config is the resolved option set. Unexported: adopters configure through
// Option funcs so defaults stay in one place and zero values never leak.
type config struct {
	trust            Trust
	maxSkills        int
	maxSkillBytes    int64
	maxCatalogBytes  int
	maxResourceBytes int64
	maxFilesPerSkill int
	maxDepth         int
	skipDirs         map[string]struct{}
	strict           bool
}

// Option configures a load. Options are applied in order; the last write
// to a given field wins.
type Option func(*config)

// TrustedSource marks the loaded skills as adopter-vouched, so their
// activation tool carries no confirmation gate. Untrusted is the default,
// so this is the only trust option: forgetting it fails closed.
//
// Pass it only for content you control: a directory you ship, an embed.FS
// compiled into the binary, or a store you write. Content from a
// repository you merely cloned is not vouched content.
func TrustedSource() Option { return func(c *config) { c.trust = Trusted } }

// MaxSkills overrides DefaultMaxSkills. Values <= 0 restore the default.
func MaxSkills(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.maxSkills = n
		}
	}
}

// MaxSkillBytes overrides DefaultMaxSkillBytes. Values <= 0 restore the default.
func MaxSkillBytes(n int64) Option {
	return func(c *config) {
		if n > 0 {
			c.maxSkillBytes = n
		}
	}
}

// MaxCatalogBytes overrides DefaultMaxCatalogBytes, capping the rendered
// catalog and therefore the per-request prompt cost. Values <= 0 restore
// the default.
func MaxCatalogBytes(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.maxCatalogBytes = n
		}
	}
}

// MaxResourceBytes overrides DefaultMaxResourceBytes. Values <= 0 restore
// the default.
func MaxResourceBytes(n int64) Option {
	return func(c *config) {
		if n > 0 {
			c.maxResourceBytes = n
		}
	}
}

// MaxFilesPerSkill overrides DefaultMaxFilesPerSkill. Values <= 0 restore
// the default.
func MaxFilesPerSkill(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.maxFilesPerSkill = n
		}
	}
}

// MaxDepth overrides DefaultMaxDepth. Values <= 0 restore the default.
func MaxDepth(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.maxDepth = n
		}
	}
}

// SkipDirs replaces the default skip list. Names match a directory's base
// name at any depth. Passing no names disables skipping entirely.
func SkipDirs(names ...string) Option {
	return func(c *config) {
		c.skipDirs = make(map[string]struct{}, len(names))
		for _, n := range names {
			c.skipDirs[n] = struct{}{}
		}
	}
}

// Strict controls whether unrecognized frontmatter keys reject a skill.
//
// The Agent Skills spec makes unknown keys a validation error, which is the
// default here: an unexpected key usually means a typo in a key that was
// supposed to do something. Set false to interoperate with producers that
// ship extensions.
func Strict(on bool) Option { return func(c *config) { c.strict = on } }

// newConfig resolves opts over the documented defaults.
func newConfig(opts []Option) config {
	c := config{
		trust:            Untrusted,
		maxSkills:        DefaultMaxSkills,
		maxSkillBytes:    DefaultMaxSkillBytes,
		maxCatalogBytes:  DefaultMaxCatalogBytes,
		maxResourceBytes: DefaultMaxResourceBytes,
		maxFilesPerSkill: DefaultMaxFilesPerSkill,
		maxDepth:         DefaultMaxDepth,
		strict:           true,
	}
	c.skipDirs = make(map[string]struct{}, len(defaultSkipDirs))
	for _, d := range defaultSkipDirs {
		c.skipDirs[d] = struct{}{}
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	return c
}

// validateName enforces the spec's name rules with a byte loop rather than
// a regexp: the check runs once per candidate directory, and avoiding the
// regexp engine keeps this package's dependency surface to yaml alone.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("skills: name is empty")
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("skills: name %q exceeds %d characters", name, MaxNameLen)
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9', ch == '-':
		default:
			return fmt.Errorf("skills: name %q contains invalid character %q", name, ch)
		}
	}
	return nil
}
