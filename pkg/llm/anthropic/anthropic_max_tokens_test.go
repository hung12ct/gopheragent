package anthropic

import "testing"

func TestWithMaxTokens_OverridesDefault(t *testing.T) {
	// We can't construct a real provider in unit tests (it requires an
	// API key), but we can verify the option function applies cleanly to
	// a zero-value provider.
	p := &Provider{MaxTokens: DefaultMaxTokens}
	WithMaxTokens(32768)(p)
	if p.MaxTokens != 32768 {
		t.Fatalf("WithMaxTokens did not apply: got %d, want 32768", p.MaxTokens)
	}
}

func TestDefaultAnthropicMaxTokens_IsAbove4096(t *testing.T) {
	// Lock in the design choice: bump from the SDK-historical 4096 to a
	// value that catches code-gen workloads without truncation. Any
	// future change to lower the default will fail this test and force a
	// conscious decision.
	if DefaultMaxTokens <= 4096 {
		t.Fatalf("DefaultMaxTokens regressed to %d — code-gen turns will truncate. Bump or document.", DefaultMaxTokens)
	}
}
