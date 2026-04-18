package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --- glob matcher ---

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"", "", true},
		{"", "x", false},
		{"*", "", true},
		{"*", "anything", true},
		{"git *", "git status", true},
		{"git *", "gitstatus", false},
		{"git ?tatus", "git status", true},
		{"git ?tatus", "git statuss", false},
		{"*github.com*", "https://api.github.com/repos", true},
		{"*github.com*", "https://gitlab.com/x", false},
		{"rm *", "rm -rf /", true},
		{"rm *", "rmdir tmp", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"a*b*c", "abc", true},
		{"*.md", "readme.md", true},
		{"*.md", "readme.txt", false},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

// --- parser ---

func TestParsePermissionRule(t *testing.T) {
	cases := []struct {
		input    string
		wantName string
		wantGlob string
	}{
		{"Bash", "Bash", ""},
		{"Bash(git status)", "Bash", "git status"},
		{"  Bash  ", "Bash", ""},
		{"WebFetch(*github.com*)", "WebFetch", "*github.com*"},
		{"ToolA(rule(with)parens)", "ToolA", "rule(with)parens"},
	}
	for _, tc := range cases {
		got, err := parsePermissionRule(tc.input)
		if err != nil {
			t.Errorf("parsePermissionRule(%q) error: %v", tc.input, err)
			continue
		}
		if got.ToolName != tc.wantName || got.ArgGlob != tc.wantGlob {
			t.Errorf("parsePermissionRule(%q) = {%q, %q}, want {%q, %q}", tc.input, got.ToolName, got.ArgGlob, tc.wantName, tc.wantGlob)
		}
	}
}

func TestParsePermissionRule_Invalid(t *testing.T) {
	invalids := []string{
		"",
		"   ",
		"Bash(missing close",
		"(no-name)",
	}
	for _, s := range invalids {
		if _, err := parsePermissionRule(s); err == nil {
			t.Errorf("parsePermissionRule(%q) expected error, got nil", s)
		}
	}
}

func TestPermissionRuleSet_PanicsOnInvalidPattern(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on invalid Allow pattern")
		}
	}()
	NewPermissionRuleSet().Allow("Bash(unterminated")
}

// --- rule-set decision logic ---

func TestPermissionRuleSet_AllowMatchesExactTool(t *testing.T) {
	r := NewPermissionRuleSet().Allow("ListFiles")
	ctx := context.Background()
	if got := r.Check(ctx, "ListFiles", `{}`); got != PermissionAllow {
		t.Fatalf("expected Allow, got %v", got)
	}
	if got := r.Check(ctx, "WriteFile", `{}`); got != PermissionPrompt {
		t.Fatalf("non-matching tool should fall through to Prompt, got %v", got)
	}
}

func TestPermissionRuleSet_AllowGlobMatchesArgs(t *testing.T) {
	r := NewPermissionRuleSet().Allow(`Bash(*"git status"*)`)
	ctx := context.Background()
	if got := r.Check(ctx, "Bash", `{"command":"git status"}`); got != PermissionAllow {
		t.Fatalf("expected Allow for git status, got %v", got)
	}
	if got := r.Check(ctx, "Bash", `{"command":"rm -rf /"}`); got != PermissionPrompt {
		t.Fatalf("expected Prompt fallthrough for rm, got %v", got)
	}
}

func TestPermissionRuleSet_DenyOverridesAllow(t *testing.T) {
	// Deny rules take precedence even when an allow rule matches too.
	r := NewPermissionRuleSet().
		Allow(`Bash(*)`).
		Deny(`Bash(*rm -rf*)`)
	ctx := context.Background()
	if got := r.Check(ctx, "Bash", `{"command":"rm -rf /"}`); got != PermissionDeny {
		t.Fatalf("Deny must override Allow, got %v", got)
	}
	if got := r.Check(ctx, "Bash", `{"command":"ls"}`); got != PermissionAllow {
		t.Fatalf("non-denied Bash call should Allow, got %v", got)
	}
}

func TestPermissionRuleSet_NoMatchFallsThroughToPrompt(t *testing.T) {
	r := NewPermissionRuleSet().
		Allow("ListFiles").
		Deny("DeleteFile")
	if got := r.Check(context.Background(), "Unrelated", `{}`); got != PermissionPrompt {
		t.Fatalf("expected Prompt for unmatched tool, got %v", got)
	}
}

func TestPermissionRuleSet_EmptyPolicyIsAlwaysPrompt(t *testing.T) {
	r := NewPermissionRuleSet()
	if got := r.Check(context.Background(), "AnyTool", `{}`); got != PermissionPrompt {
		t.Fatalf("empty policy must default to Prompt, got %v", got)
	}
}

// --- integration with RunIteration ---

func TestPermissions_AllowBypassesHITL(t *testing.T) {
	// A HITL-gated tool with NO ConfirmHITL callback would normally be
	// auto-denied. An Allow rule must bypass that and let the tool run.
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "dangerous", ArgsJSON: `{"cmd":"ls"}`}}},
		{Content: "done"},
	}}
	loop, _ := setup(provider, &echoTool{name: "dangerous", confirm: true})
	loop.Permissions = NewPermissionRuleSet().Allow("dangerous")
	// Intentionally leave ConfirmHITL nil to prove Allow skips the prompt.

	resp, err := loop.RunIteration(context.Background(), "s1", "go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "done" {
		t.Fatalf("expected 'done' after auto-approved tool, got %q", resp)
	}
}

func TestPermissions_DenyBlocksWithoutHITLPrompt(t *testing.T) {
	// Deny must block even a non-HITL tool — no ConfirmHITL invocation,
	// a structured PermissionDeniedError surfaces to the model.
	hitlCalls := 0
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "echo", ArgsJSON: `{"cmd":"rm -rf /"}`}}},
		{Content: "found workaround"},
	}}
	loop, _ := setup(provider, &echoTool{name: "echo"})
	loop.ConfirmHITL = func(_ context.Context, _, _ string) bool {
		hitlCalls++
		return true
	}
	loop.Permissions = NewPermissionRuleSet().Deny(`echo(*"rm -rf"*)`)

	resp, err := loop.RunIteration(context.Background(), "s1", "go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hitlCalls != 0 {
		t.Fatalf("Deny must not invoke ConfirmHITL, got %d calls", hitlCalls)
	}
	if resp != "found workaround" {
		t.Fatalf("expected 'found workaround', got %q", resp)
	}
}

func TestPermissions_DenyErrorIsStructured(t *testing.T) {
	// Downstream code can pattern-match on PermissionDeniedError /
	// ErrPermissionDenied to distinguish policy denies from HITL denies.
	pde := &PermissionDeniedError{ToolName: "dangerous"}
	if !errors.Is(pde, ErrPermissionDenied) {
		t.Fatal("PermissionDeniedError must match ErrPermissionDenied via errors.Is")
	}
	if !strings.Contains(pde.Error(), "dangerous") {
		t.Fatalf("error text must include tool name, got %q", pde.Error())
	}
}

func TestPermissions_PromptFallsThroughToExistingHITL(t *testing.T) {
	// When the policy is silent (PermissionPrompt), the existing HITL
	// flow runs unchanged — ConfirmHITL is called.
	hitlCalls := 0
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "dangerous", ArgsJSON: `{}`}}},
		{Content: "executed"},
	}}
	loop, _ := setup(provider, &echoTool{name: "dangerous", confirm: true})
	loop.Permissions = NewPermissionRuleSet().Allow("OtherTool") // no match
	loop.ConfirmHITL = func(_ context.Context, _, _ string) bool {
		hitlCalls++
		return true
	}

	resp, err := loop.RunIteration(context.Background(), "s1", "go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hitlCalls != 1 {
		t.Fatalf("expected ConfirmHITL to fire once on Prompt fallthrough, got %d", hitlCalls)
	}
	if resp != "executed" {
		t.Fatalf("expected 'executed' after human approval, got %q", resp)
	}
}

func TestPermissions_NilPermissionsDoesNotBreakExistingFlow(t *testing.T) {
	// Regression guard: a loop with Permissions == nil must behave
	// identically to the old pre-DSL behavior.
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "echo", ArgsJSON: `{}`}}},
		{Content: "done"},
	}}
	loop, _ := setup(provider, &echoTool{name: "echo"})
	// loop.Permissions intentionally unset.

	resp, err := loop.RunIteration(context.Background(), "s1", "go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "done" {
		t.Fatalf("expected 'done', got %q", resp)
	}
}
