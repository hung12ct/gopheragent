package skills

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// frontmatter is the Agent Skills frontmatter block.
//
// The yaml tags are exhaustive on purpose: strict parsing runs the decoder
// with KnownFields(true), so any key absent from this struct rejects the
// skill. Adding a spec field means adding it here.
type frontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license"`
	Compatibility string            `yaml:"compatibility"`
	Metadata      map[string]string `yaml:"metadata"`
	AllowedTools  string            `yaml:"allowed-tools"`
}

// parseSkillDoc splits a SKILL.md into validated frontmatter and body.
//
// This deliberately does NOT share code with the OKF frontmatter parser in
// pkg/builder, despite the similar shape. The two need opposite behavior in
// both dimensions that matter:
//
//   - Strictness. OKF's contract is that consumers must not reject
//     documents over unrecognized fields; Agent Skills makes unknown keys a
//     validation error. Sharing means one of them is wrong, and the wrong
//     one would be a security control.
//   - Failure mode. The OKF parser returns content verbatim when YAML is
//     malformed, so plain Markdown using "---" as a horizontal rule passes
//     through untouched. A malformed SKILL.md must be rejected instead.
//     Coupled, a future OKF leniency fix would silently loosen skill
//     validation.
//
// Twenty duplicated lines is the cheaper side of that trade.
func parseSkillDoc(content string, strict bool) (fm frontmatter, body string, lenient bool, err error) {
	block, body, err := splitFence(content)
	if err != nil {
		return frontmatter{}, "", false, err
	}

	dec := yaml.NewDecoder(strings.NewReader(block))
	dec.KnownFields(strict)
	if decErr := dec.Decode(&fm); decErr == nil {
		return fm, body, false, nil
	} else if strict && isUnknownFieldErr(decErr) {
		// An unrecognized key is a typo in a key meant to do something,
		// not a parse failure the fallback should paper over.
		return frontmatter{}, "", false, fmt.Errorf("skills: frontmatter: %w", decErr)
	}

	// The spec's own idiom "description: Use when: the user asks about X"
	// is not valid YAML, and it appears throughout published skills. Fall
	// back to a line scanner rather than reject those outright.
	fm, ok := scanLenient(block)
	if !ok {
		return frontmatter{}, "", false, fmt.Errorf("skills: frontmatter: no parseable name and description")
	}
	return fm, body, true, nil
}

// splitFence separates a leading "---" fenced block from the body.
//
// Unlike the OKF parser, a missing or unterminated fence is an error: a
// SKILL.md without frontmatter has no name and no description, so there is
// nothing to recover and silently treating it as a bodyless document would
// hide the real problem.
func splitFence(content string) (block, body string, err error) {
	s := strings.TrimPrefix(content, "\ufeff") // tolerate a UTF-8 BOM
	lines := strings.Split(s, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return "", "", fmt.Errorf("skills: frontmatter: missing opening --- fence")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			block = strings.Join(lines[1:i], "\n")
			body = strings.TrimLeft(strings.Join(lines[i+1:], "\n"), "\r\n")
			return block, body, nil
		}
	}
	return "", "", fmt.Errorf("skills: frontmatter: missing closing --- fence")
}

// isUnknownFieldErr reports whether err is yaml.v3's KnownFields rejection.
// The library reports it as a plain *yaml.TypeError with no sentinel, so
// matching on the message is the only option available.
func isUnknownFieldErr(err error) bool {
	return strings.Contains(err.Error(), "field ") && strings.Contains(err.Error(), "not found in type")
}

// scanLenient recovers name and description from frontmatter that failed
// strict YAML parsing.
//
// It deliberately recovers NOTHING else. A block whose YAML does not parse
// is precisely where a privilege grant (allowed-tools) or arbitrary
// metadata must not be read out of half-understood text — the parser has
// already demonstrated it does not know where values end. Callers surface
// this via Skill.Lenient.
//
// Returns ok=false when either required field is missing.
func scanLenient(block string) (fm frontmatter, ok bool) {
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		// Only column-0 keys: an indented line belongs to some nested
		// structure this scanner has no business interpreting.
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			fm.Name = unquote(strings.TrimSpace(value))
		case "description":
			fm.Description = unquote(strings.TrimSpace(value))
		}
	}
	return fm, fm.Name != "" && fm.Description != ""
}

// unquote strips one matched pair of surrounding quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// validate enforces the field bounds.
//
// The name-equals-directory rule is NOT checked here: it only applies to
// skills loaded from an fs.FS, and in-memory skills built by New must run
// the same field validation without it. FromFS applies that rule itself.
func (fm frontmatter) validate() error {
	if err := validateName(fm.Name); err != nil {
		return err
	}
	if fm.Description == "" {
		return fmt.Errorf("skills: %s: description is empty", fm.Name)
	}
	if len(fm.Description) > MaxDescriptionLen {
		return fmt.Errorf("skills: %s: description exceeds %d characters", fm.Name, MaxDescriptionLen)
	}
	if len(fm.Compatibility) > MaxCompatibilityLen {
		return fmt.Errorf("skills: %s: compatibility exceeds %d characters", fm.Name, MaxCompatibilityLen)
	}
	if len(fm.Metadata) > MaxMetadataKeys {
		return fmt.Errorf("skills: %s: metadata has %d keys, limit %d", fm.Name, len(fm.Metadata), MaxMetadataKeys)
	}
	for k, v := range fm.Metadata {
		if len(v) > MaxMetadataValueLen {
			return fmt.Errorf("skills: %s: metadata %q exceeds %d characters", fm.Name, k, MaxMetadataValueLen)
		}
	}
	return nil
}
