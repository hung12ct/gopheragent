package builder

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/hung12ct/gopheragent/pkg/skills"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"github.com/hung12ct/gopheragent/pkg/tools/builtin"
	"gopkg.in/yaml.v3"
)

// SkillsConfig is the agent.skills block.
//
//	agent:
//	  skills:
//	    sources:
//	      - ./skills                  # shorthand: trusted
//	      - dir: ./vendor-skills
//	        trust: untrusted
//	    max_catalog_bytes: 16384      # optional
type SkillsConfig struct {
	Sources []SkillSource `yaml:"sources"`
	// Bound overrides. Zero uses the pkg/skills defaults, which is what
	// almost every deployment should do — see that package for the figures
	// and for why the catalog bound is the one that matters.
	MaxSkills       int   `yaml:"max_skills,omitempty"`
	MaxSkillBytes   int64 `yaml:"max_skill_bytes,omitempty"`
	MaxCatalogBytes int   `yaml:"max_catalog_bytes,omitempty"`
}

// SkillSource is one directory of skills. It accepts either a bare string
// or a mapping:
//
//	sources:
//	  - ./skills                  # shorthand
//	  - dir: ./vendor
//	    trust: untrusted
type SkillSource struct {
	// Dir is a path relative to the YAML file's directory, or absolute.
	Dir string `yaml:"dir"`
	// Trust is "trusted" (the default) or "untrusted".
	//
	// The shorthand form defaults to trusted, unlike skills.FromFS, which
	// defaults to untrusted. A path typed into the operator's own config
	// file is operator-authored by definition; making them write trust:
	// trusted on every entry would produce "I configured skills and got
	// nothing" for the common case, and a bound nobody reads is a bound
	// nobody keeps. Mark a source untrusted when it comes from somewhere
	// you did not write.
	Trust string `yaml:"trust,omitempty"`
}

// UnmarshalYAML accepts both the scalar shorthand and the mapping form.
func (s *SkillSource) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var dir string
		if err := node.Decode(&dir); err != nil {
			return fmt.Errorf("builder: agent.skills.sources: %w", err)
		}
		s.Dir = dir
		s.Trust = trustTrusted
		return nil
	}
	// A named type avoids recursing into this method.
	type rawSource SkillSource
	var raw rawSource
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("builder: agent.skills.sources: %w", err)
	}
	*s = SkillSource(raw)
	if s.Trust == "" {
		s.Trust = trustTrusted
	}
	return nil
}

const (
	trustTrusted   = "trusted"
	trustUntrusted = "untrusted"
)

// WithSkillCatalog returns basePrompt with the skill catalog appended after
// a blank line, mirroring WithKnowledgeBase.
//
// The catalog belongs in the system prompt rather than in a per-request
// injection: it is fixed for the process lifetime, so it sits inside the
// stable prefix an Anthropic cache breakpoint covers. It is also why this
// does not go through agent.DynamicContext — that is a single slot, and
// claiming it here would silently displace whatever the adopter put there.
//
// Returns basePrompt unchanged when set is nil or empty.
func WithSkillCatalog(basePrompt string, set *skills.Set) string {
	return joinPromptKB(basePrompt, set.Catalog())
}

// resolveSkills loads every configured source and merges them.
//
// Sources merge in declaration order and the first to claim a name wins, so
// listing trusted sources first is what stops a skill from a directory you
// do not control shadowing one you do.
func resolveSkills(ctx context.Context, cfg *SkillsConfig, baseDir string) (*skills.Set, error) {
	if cfg == nil || len(cfg.Sources) == 0 {
		return nil, nil
	}
	sets := make([]*skills.Set, 0, len(cfg.Sources))
	for _, src := range cfg.Sources {
		dir := src.Dir
		if !filepath.IsAbs(dir) {
			if baseDir == "" {
				return nil, fmt.Errorf("agent.skills.sources dir %q is relative but no baseDir was provided — pass baseDir or use an absolute path", dir)
			}
			dir = filepath.Join(baseDir, dir)
		}
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("agent.skills.sources dir %q: %w", src.Dir, err)
		}
		set, err := skills.FromFS(ctx, os.DirFS(dir), skillOptions(cfg, src)...)
		if err != nil {
			return nil, fmt.Errorf("agent.skills.sources dir %q: %w", src.Dir, err)
		}
		sets = append(sets, set)
	}
	merged, err := skills.Merge(sets...)
	if err != nil {
		return nil, fmt.Errorf("agent.skills: %w", err)
	}
	return merged, nil
}

// skillOptions translates the YAML block into loader options.
func skillOptions(cfg *SkillsConfig, src SkillSource) []skills.Option {
	opts := make([]skills.Option, 0, 4)
	if src.Trust != trustUntrusted {
		opts = append(opts, skills.TrustedSource())
	}
	if cfg.MaxSkills > 0 {
		opts = append(opts, skills.MaxSkills(cfg.MaxSkills))
	}
	if cfg.MaxSkillBytes > 0 {
		opts = append(opts, skills.MaxSkillBytes(cfg.MaxSkillBytes))
	}
	if cfg.MaxCatalogBytes > 0 {
		opts = append(opts, skills.MaxCatalogBytes(cfg.MaxCatalogBytes))
	}
	return opts
}

// catalogRegisterer registers tools through the global catalog's middleware
// before they reach the agent registry.
//
// builtin.RegisterSkillTools builds its own tools, so it cannot go through
// GlobalCatalog.Get like tools_required entries do. Without this adapter the
// skill tools would be the only ones in the registry that middleware
// registered with GlobalCatalog.Use never sees — otel spans would simply
// stop at skill activations, which is the kind of gap nobody notices until
// they are debugging something else.
type catalogRegisterer struct {
	catalog *GlobalCatalog
	target  tools.Registerer
}

func (c catalogRegisterer) Register(t tools.Tool) {
	if c.catalog != nil {
		t = c.catalog.wrap(t)
	}
	c.target.Register(t)
}

// registerSkillTools wires the activation tools into registry, wrapped in
// the catalog's middleware, and warns once about the Selector interaction.
func registerSkillTools(registry *tools.Registry, catalog *GlobalCatalog, set *skills.Set) {
	if set.Len() == 0 {
		return
	}
	builtin.RegisterSkillTools(catalogRegisterer{catalog: catalog, target: registry}, set)

	for _, s := range set.Skipped() {
		log.Printf("[gopheragent] skill skipped: %s", s)
	}
	// The builder cannot pin for the adopter: a tools.Selector is built
	// after the loop, so WithPinned is out of reach from here. Say so once
	// at startup instead of letting activation vanish mid-conversation.
	log.Printf("[gopheragent] loaded %d skill(s); if you attach a tools.Selector, pin %v or skill activation will intermittently disappear",
		set.Len(), builtin.SkillToolNames())
}

// validateSkillsConfig appends any problems in the skills block to issues.
func validateSkillsConfig(cfg *SkillsConfig, issues []string) []string {
	if cfg == nil {
		return issues
	}
	if len(cfg.Sources) == 0 {
		return append(issues, "agent.skills.sources is empty — list at least one directory or remove the skills block")
	}
	for i, src := range cfg.Sources {
		if src.Dir == "" {
			issues = append(issues, fmt.Sprintf("agent.skills.sources[%d].dir is required", i))
		}
		switch src.Trust {
		case "", trustTrusted, trustUntrusted:
		default:
			issues = append(issues, fmt.Sprintf("agent.skills.sources[%d].trust is %q — must be %q or %q", i, src.Trust, trustTrusted, trustUntrusted))
		}
	}
	return issues
}
