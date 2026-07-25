package skills

import (
	"fmt"
	"io/fs"
)

// Trust records whether the adopter vouched for a skill's content.
//
// Untrusted is the zero value so a forgotten option fails closed. Trust is
// not inferred from where a skill was found — the adopter chose which fs.FS
// to hand to FromFS and says so explicitly with the Trusted option. That is
// the whole reason this package takes an fs.FS instead of a directory path:
// provenance is the caller's knowledge, not something to guess from a path.
type Trust uint8

const (
	// Untrusted content may still be loaded, but its activation tool is
	// HITL-gated so a human sees the instructions before the model does.
	Untrusted Trust = iota
	// Trusted content activates without a confirmation prompt.
	Trusted
)

// String implements fmt.Stringer for log and error messages.
func (t Trust) String() string {
	if t == Trusted {
		return "trusted"
	}
	return "untrusted"
}

// Skill is one validated SKILL.md and the resource files beside it.
// Immutable once a Set is built.
type Skill struct {
	// Name is the skill identifier the model activates by. Validated to
	// 1..MaxNameLen characters of [A-Za-z0-9-], and required to equal the
	// containing directory's name so the catalog cannot advertise a skill
	// under a name that does not resolve.
	Name string
	// Description tells the model what the skill does and when to reach
	// for it. This is the only skill text in the system prompt, so it
	// carries the entire activation decision.
	Description string
	// Body is the SKILL.md content with frontmatter stripped, loaded
	// eagerly (see the Set doc for why).
	Body string
	// Location is the slash-separated path of the skill directory within
	// its source fs.FS. Rendered in the catalog so the model can refer to
	// bundled files; deliberately not an absolute host path.
	Location string
	// Files lists resource paths relative to the skill directory, sorted.
	// This doubles as the allowlist Set.File validates against — a path
	// that is not in here is rejected before any syscall.
	Files []string
	// Trust is inherited from the load option that produced this skill.
	Trust Trust
	// License and Compatibility carry the optional spec fields verbatim.
	License       string
	Compatibility string
	// Metadata is the optional free-form frontmatter map.
	Metadata map[string]string
	// AllowedTools is ADVISORY ONLY and is never consulted by this package
	// or by the activation tools.
	//
	// It is a privilege grant authored by the skill itself. Honoring it
	// would let content decide its own permissions, which is the whole
	// point of not doing so. Adopters may intersect it with their registry
	// to RESTRICT what a skill may call; never use it to GRANT.
	AllowedTools []string
	// Lenient is true when strict YAML parsing failed and the fallback
	// scanner recovered this skill from Name and Description alone. Every
	// other optional field is empty in that case — see frontmatter.go.
	Lenient bool

	// fsys is the source Set.File reads resources from, set by FromFS.
	// Unexported so a hand-constructed Skill cannot aim File at an
	// arbitrary filesystem; skills built by New have none, and report
	// that they expose no files.
	fsys fs.FS
}

// SkipReason explains why a candidate directory did not become a Skill.
type SkipReason uint8

const (
	// SkipInvalidFrontmatter covers a missing fence, unparseable YAML that
	// the lenient fallback could not rescue, and failed field validation.
	SkipInvalidFrontmatter SkipReason = iota
	// SkipNameMismatch means frontmatter name != directory name.
	SkipNameMismatch
	// SkipDuplicateName means an earlier source already claimed the name.
	SkipDuplicateName
	// SkipOversize means SKILL.md exceeded MaxSkillBytes.
	SkipOversize
	// SkipBudgetExhausted means MaxSkills or MaxCatalogBytes was reached
	// before this skill could be admitted. MaxSkills short-circuits before
	// SKILL.md is opened; MaxCatalogBytes cannot, since the cost depends on
	// the name and description inside the file — but the body is discarded
	// either way, so neither bound is exceeded in memory.
	SkipBudgetExhausted
	// SkipUnreadable covers I/O errors on the directory or SKILL.md.
	SkipUnreadable
)

// String implements fmt.Stringer so log lines read plainly.
func (r SkipReason) String() string {
	switch r {
	case SkipInvalidFrontmatter:
		return "invalid frontmatter"
	case SkipNameMismatch:
		return "name does not match directory"
	case SkipDuplicateName:
		return "duplicate name"
	case SkipOversize:
		return "exceeds size limit"
	case SkipBudgetExhausted:
		return "budget exhausted"
	case SkipUnreadable:
		return "unreadable"
	default:
		return fmt.Sprintf("unknown reason %d", uint8(r))
	}
}

// Skipped records a rejected candidate.
//
// Loading never fails the whole call over one bad skill — a malformed
// SKILL.md must not break agent startup. That trade only works if the
// rejections are reportable, otherwise "my skill isn't loading" has no
// diagnosis. Log Set.Skipped() once at startup.
type Skipped struct {
	// Location is the candidate directory within its source fs.FS.
	Location string
	// Name is best-effort: empty when the frontmatter could not be read.
	Name string
	// Reason is the machine-readable rejection cause.
	Reason SkipReason
	// Err carries the underlying failure for I/O and parse rejections;
	// nil for duplicate-name and budget rejections, which are not errors.
	Err error
}

// String renders a one-line diagnostic suitable for a startup log.
func (s Skipped) String() string {
	if s.Err != nil {
		return fmt.Sprintf("%s: %s: %v", s.Location, s.Reason, s.Err)
	}
	return fmt.Sprintf("%s: %s", s.Location, s.Reason)
}
