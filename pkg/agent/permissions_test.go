package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
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

func TestPermissionRuleSet_ReturnsErrorOnInvalidPattern(t *testing.T) {
	if _, err := NewPermissionRuleSet([]string{"Bash(unterminated"}, nil); err == nil {
		t.Fatal("expected error on invalid Allow pattern, got nil")
	}
	if _, err := NewPermissionRuleSet(nil, []string{"Bash(unterminated"}); err == nil {
		t.Fatal("expected error on invalid Deny pattern, got nil")
	}
}

// --- rule-set decision logic ---

func TestPermissionRuleSet_AllowMatchesExactTool(t *testing.T) {
	r, err := NewPermissionRuleSet([]string{"ListFiles"}, nil)
	if err != nil {
		t.Fatalf("NewPermissionRuleSet: %v", err)
	}
	ctx := context.Background()
	if got := r.Check(ctx, "ListFiles", `{}`); got != PermissionAllow {
		t.Fatalf("expected Allow, got %v", got)
	}
	if got := r.Check(ctx, "WriteFile", `{}`); got != PermissionPrompt {
		t.Fatalf("non-matching tool should fall through to Prompt, got %v", got)
	}
}

func TestPermissionRuleSet_AllowGlobMatchesArgs(t *testing.T) {
	r, err := NewPermissionRuleSet([]string{`Bash(*"git status"*)`}, nil)
	if err != nil {
		t.Fatalf("NewPermissionRuleSet: %v", err)
	}
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
	r, err := NewPermissionRuleSet([]string{`Bash(*)`}, []string{`Bash(*rm -rf*)`})
	if err != nil {
		t.Fatalf("NewPermissionRuleSet: %v", err)
	}
	ctx := context.Background()
	if got := r.Check(ctx, "Bash", `{"command":"rm -rf /"}`); got != PermissionDeny {
		t.Fatalf("Deny must override Allow, got %v", got)
	}
	if got := r.Check(ctx, "Bash", `{"command":"ls"}`); got != PermissionAllow {
		t.Fatalf("non-denied Bash call should Allow, got %v", got)
	}
}

func TestPermissionRuleSet_NoMatchFallsThroughToPrompt(t *testing.T) {
	r, err := NewPermissionRuleSet([]string{"ListFiles"}, []string{"DeleteFile"})
	if err != nil {
		t.Fatalf("NewPermissionRuleSet: %v", err)
	}
	if got := r.Check(context.Background(), "Unrelated", `{}`); got != PermissionPrompt {
		t.Fatalf("expected Prompt for unmatched tool, got %v", got)
	}
}

func TestPermissionRuleSet_EmptyPolicyIsAlwaysPrompt(t *testing.T) {
	r, err := NewPermissionRuleSet(nil, nil)
	if err != nil {
		t.Fatalf("NewPermissionRuleSet: %v", err)
	}
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
	perms, _ := NewPermissionRuleSet([]string{"dangerous"}, nil)
	loop.Permissions = perms
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
	perms, _ := NewPermissionRuleSet(nil, []string{`echo(*"rm -rf"*)`})
	loop.Permissions = perms

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
	perms, _ := NewPermissionRuleSet([]string{"OtherTool"}, nil) // no match
	loop.Permissions = perms
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

func TestPermissionRuleSet_ConfirmEscalatesUngatedCall(t *testing.T) {
	rules, err := NewPermissionRuleSet(nil, nil)
	if err != nil {
		t.Fatalf("NewPermissionRuleSet: %v", err)
	}
	if err := rules.AddConfirm(`write_file(*"/etc/*"*)`); err != nil {
		t.Fatalf("AddConfirm: %v", err)
	}
	ctx := context.Background()

	if got := rules.Check(ctx, "write_file", `{"path":"/etc/hosts"}`); got != PermissionConfirm {
		t.Fatalf("matching call should escalate, got %v", got)
	}
	if got := rules.Check(ctx, "write_file", `{"path":"/tmp/scratch"}`); got != PermissionPrompt {
		t.Fatalf("non-matching call should fall through, got %v", got)
	}
	if got := rules.Check(ctx, "read_file", `{"path":"/etc/hosts"}`); got != PermissionPrompt {
		t.Fatalf("different tool should not match, got %v", got)
	}
}

// Precedence: Deny beats everything, an explicit Allow narrows a broad
// Confirm. Without this ordering a Confirm rule could not be excepted.
func TestPermissionRuleSet_ConfirmPrecedence(t *testing.T) {
	rules, err := NewPermissionRuleSet(nil, nil)
	if err != nil {
		t.Fatalf("NewPermissionRuleSet: %v", err)
	}
	if err := rules.AddConfirm("write_file"); err != nil {
		t.Fatalf("AddConfirm: %v", err)
	}
	if err := rules.AddAllow(`write_file(*"/tmp/*"*)`); err != nil {
		t.Fatalf("AddAllow: %v", err)
	}
	if err := rules.AddDeny(`write_file(*"/etc/shadow"*)`); err != nil {
		t.Fatalf("AddDeny: %v", err)
	}
	ctx := context.Background()

	cases := []struct {
		args string
		want PermissionDecision
	}{
		{`{"path":"/etc/shadow"}`, PermissionDeny},
		{`{"path":"/tmp/scratch"}`, PermissionAllow},
		{`{"path":"/var/lib/thing"}`, PermissionConfirm},
	}
	for _, tc := range cases {
		if got := rules.Check(ctx, "write_file", tc.args); got != tc.want {
			t.Errorf("Check(%s) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// The point of the whole change, driven through the real loop rather than a
// restatement of its gate condition: a tool whose descriptor does NOT
// declare RequiresConfirmation must still reach the HITL prompt when policy
// escalates it. Before PermissionConfirm this was impossible — a policy
// could restrict a tool that already prompted but never escalate one that
// did not, so the only workaround was registering the tool twice.
func TestPermissionConfirm_FiresHITLForUngatedTool(t *testing.T) {
	run := func(t *testing.T, argsJSON string) (prompted []string, output string) {
		t.Helper()
		rules, err := NewPermissionRuleSet(nil, nil)
		if err != nil {
			t.Fatalf("NewPermissionRuleSet: %v", err)
		}
		if err := rules.AddConfirm(`danger(*production*)`); err != nil {
			t.Fatalf("AddConfirm: %v", err)
		}

		reg := tools.NewRegistry()
		reg.Register(&echoTool{name: "danger"}) // confirm:false — ungated descriptor

		provider := &scriptProvider{turns: []LLMResult{
			{ToolCalls: []PendingToolCall{{ID: "1", Name: "danger", ArgsJSON: argsJSON}}},
			{Content: "finished"},
		}}
		sm := history.NewInMemSessionManager("sys")
		loop := NewAgentLoop(sm, reg, provider)
		loop.Permissions = rules
		loop.ConfirmHITL = func(_ context.Context, toolName, args string) bool {
			prompted = append(prompted, toolName+" "+args)
			return true
		}

		msg := history.Message{Role: "user", Content: "go"}
		for ev := range loop.Run(context.Background(), "sess", msg) {
			if c, ok := ev.Payload.(ContentEvent); ok {
				output += c.Text
			}
		}
		return prompted, output
	}

	t.Run("matching args reach the gate", func(t *testing.T) {
		prompted, _ := run(t, `{"env":"production"}`)
		if len(prompted) != 1 {
			t.Fatalf("expected exactly one HITL prompt, got %v", prompted)
		}
		if !strings.Contains(prompted[0], "danger") {
			t.Fatalf("wrong tool prompted: %v", prompted)
		}
	})

	t.Run("non-matching args stay ungated", func(t *testing.T) {
		prompted, _ := run(t, `{"env":"staging"}`)
		if len(prompted) != 0 {
			t.Fatalf("a call the policy did not escalate must not prompt, got %v", prompted)
		}
	})
}
