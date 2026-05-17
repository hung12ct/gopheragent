package agent

import (
	"context"
	"fmt"
	"strings"
)

// PermissionDecision is the outcome of a per-call permission check.
//
// The default zero value is PermissionPrompt so a nil / unopinionated
// check behaves as if no policy existed and the loop falls back to the
// tool's own RequiresConfirmation() flow.
type PermissionDecision int

const (
	// PermissionPrompt means the policy has no opinion — the loop
	// defers to the tool's RequiresConfirmation() and ConfirmHITL.
	PermissionPrompt PermissionDecision = iota
	// PermissionAllow auto-approves the call, bypassing HITL even when
	// the tool declares RequiresConfirmation() = true. Use with care:
	// this is the knob that turns a "human every 30 seconds" agent
	// into one you can leave running overnight.
	PermissionAllow
	// PermissionDeny blocks the call and surfaces a structured error to
	// the model, bypassing HITL entirely. Deny overrides everything,
	// including tools that do not declare RequiresConfirmation().
	PermissionDeny
)

// PermissionChecker is consulted before the HITL prompt on every tool
// call. Implementations must be safe for concurrent use — the loop may
// run multiple tool calls in parallel within a single wave.
type PermissionChecker interface {
	Check(ctx context.Context, toolName, argsJSON string) PermissionDecision
}

// PermissionRuleSet is a deny-over-allow policy composed of glob
// patterns. It implements PermissionChecker so it can be assigned to
// AgentLoop.Permissions directly.
//
// Rule syntax: "ToolName" or "ToolName(glob)". The glob supports the
// usual shell-style * (any sequence) and ? (any single character); no
// character classes or ** syntax. When the glob is absent, the rule
// matches every invocation of ToolName. Globs are matched against the
// raw argsJSON — implement PermissionChecker directly if you need
// structured (field-aware) matching.
//
// Evaluation order: Deny rules are checked first, then Allow. The
// first match wins; if no rule matches the decision is
// PermissionPrompt and the loop falls back to HITL.
//
//	rules := agent.NewPermissionRuleSet().
//	    Allow(`Bash(*"git status"*)`, `WebFetch(*github.com*)`).
//	    Deny(`Bash(*"rm -rf"*)`, `Bash(*"sudo "*)`)
//	loop.Permissions = rules
type PermissionRuleSet struct {
	allow []permissionRule
	deny  []permissionRule
}

// permissionRule is a parsed Allow / Deny pattern. ArgGlob == "" means
// the rule matches every invocation of ToolName regardless of args.
type permissionRule struct {
	ToolName string
	ArgGlob  string // "" means "match any args"
}

// NewPermissionRuleSet parses allow + deny pattern slices and returns a
// ready-to-use ruleset. Returns an error when any pattern is malformed —
// adopters loading rules from YAML / env / API input get a typed failure
// instead of a panic.
//
// Patterns use the shape "ToolName" or "ToolName(glob)" where glob accepts
// * (any sequence) and ? (any single character). Empty input slices produce
// an empty ruleset that Check always evaluates to PermissionPrompt.
//
//	rs, err := agent.NewPermissionRuleSet(
//	    []string{"read_file:/etc/*"},
//	    []string{"write_file:*"},
//	)
//	if err != nil { return err }
func NewPermissionRuleSet(allow, deny []string) (*PermissionRuleSet, error) {
	rs := &PermissionRuleSet{}
	parsed, err := parseRules(allow)
	if err != nil {
		return nil, fmt.Errorf("agent: NewPermissionRuleSet: allow: %w", err)
	}
	rs.allow = parsed
	parsed, err = parseRules(deny)
	if err != nil {
		return nil, fmt.Errorf("agent: NewPermissionRuleSet: deny: %w", err)
	}
	rs.deny = parsed
	return rs, nil
}

// AddAllow appends one or more patterns to the allow list. Returns an error
// when any pattern is malformed. Concurrency note: safe to call before the
// ruleset is shared with an AgentLoop; callers mutating live policies should
// guard their own writes.
func (r *PermissionRuleSet) AddAllow(patterns ...string) error {
	parsed, err := parseRules(patterns)
	if err != nil {
		return fmt.Errorf("agent: AddAllow: %w", err)
	}
	r.allow = append(r.allow, parsed...)
	return nil
}

// AddDeny appends one or more patterns to the deny list. Deny takes
// precedence over Allow at Check time.
func (r *PermissionRuleSet) AddDeny(patterns ...string) error {
	parsed, err := parseRules(patterns)
	if err != nil {
		return fmt.Errorf("agent: AddDeny: %w", err)
	}
	r.deny = append(r.deny, parsed...)
	return nil
}

// Check returns PermissionDeny / PermissionAllow / PermissionPrompt for
// the given call. It is safe for concurrent use once all rules have
// been registered — internal slices are only appended to by Allow/Deny.
func (r *PermissionRuleSet) Check(_ context.Context, toolName, argsJSON string) PermissionDecision {
	for _, rule := range r.deny {
		if rule.matches(toolName, argsJSON) {
			return PermissionDeny
		}
	}
	for _, rule := range r.allow {
		if rule.matches(toolName, argsJSON) {
			return PermissionAllow
		}
	}
	return PermissionPrompt
}

func (rule permissionRule) matches(toolName, argsJSON string) bool {
	if rule.ToolName != toolName {
		return false
	}
	if rule.ArgGlob == "" {
		return true
	}
	return globMatch(rule.ArgGlob, argsJSON)
}

// parsePermissionRule accepts "ToolName" or "ToolName(glob)" and
// returns the parsed rule. The glob section uses the last ) in the
// string as the closer so patterns containing a literal ( are OK as
// long as they end with a matching ).
func parsePermissionRule(s string) (permissionRule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return permissionRule{}, fmt.Errorf("agent: empty permission rule")
	}
	open := strings.Index(s, "(")
	if open < 0 {
		return permissionRule{ToolName: s}, nil
	}
	if !strings.HasSuffix(s, ")") {
		return permissionRule{}, fmt.Errorf("agent: permission rule %q missing closing ')'", s)
	}
	name := strings.TrimSpace(s[:open])
	if name == "" {
		return permissionRule{}, fmt.Errorf("agent: permission rule %q has empty tool name", s)
	}
	return permissionRule{ToolName: name, ArgGlob: s[open+1 : len(s)-1]}, nil
}

func parseRules(patterns []string) ([]permissionRule, error) {
	out := make([]permissionRule, 0, len(patterns))
	for _, p := range patterns {
		rule, err := parsePermissionRule(p)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, nil
}

// globMatch reports whether s matches pattern. Supports * (any
// sequence, including empty) and ? (any single character). All other
// bytes are literal — no character classes, no escaping. The two-
// pointer algorithm runs in O(len(pattern)+len(s)) amortized.
func globMatch(pattern, s string) bool {
	pi, si := 0, 0
	starPi, starSi := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == s[si] || pattern[pi] == '?'):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			starPi = pi
			starSi = si
			pi++
		case starPi >= 0:
			pi = starPi + 1
			starSi++
			si = starSi
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
