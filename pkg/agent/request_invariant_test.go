package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// violationRecorder collects invariant reports so a test can assert on them.
type violationRecorder struct {
	mu   sync.Mutex
	errs []error
}

func (v *violationRecorder) record(_ context.Context, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.errs = append(v.errs, err)
}

func (v *violationRecorder) all() []error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]error(nil), v.errs...)
}

// A noisy invariant is a useless one. Exercise the paths that legitimately
// reshape a request — plan mode prepends a system message, dynamic context
// appends a user message, the budget policy prunes and truncates — and
// require silence on all of them.
func TestRequestInvariant_CleanRunReportsNothing(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*AgentLoop)
	}{
		{"defaults", func(*AgentLoop) {}},
		{"budget forces pruning", func(al *AgentLoop) { al.MaxTokenBudget = 1 }},
		{"dynamic context injects a user message", func(al *AgentLoop) {
			al.DynamicContext = func(context.Context, string) string { return "extra grounding" }
		}},
		{"memory notes rewrite the system message", func(al *AgentLoop) {
			al.MaxTokenBudget = 40
		}},
		{"parallel cap binds", func(al *AgentLoop) {
			// A wave wider than the cap serialises some calls, so results
			// land out of dispatch order. They must still be committed in
			// model order for the next turn's derivation to match.
			al.MaxParallelToolCalls = 2
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &violationRecorder{}
			ct := &countingTool{name: "counter"}
			provider := &scriptProvider{turns: []LLMResult{
				{ToolCalls: fanoutCalls(5)},
				{Content: "final"},
			}}
			loop, _ := setup(provider, ct)
			WithRequestInvariant(rec.record)(loop)
			tc.apply(loop)

			if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, got := range rec.all() {
				t.Errorf("unexpected violation: %v", got)
			}
		})
	}
}

func TestRequestInvariant_DisabledTakesNoSnapshot(t *testing.T) {
	loop, _ := setup(&scriptProvider{turns: []LLMResult{{Content: "hi"}}})

	if snap := loop.snapshotRequest([]history.Message{{Role: "user", Content: "x"}}); snap != nil {
		t.Fatal("snapshotRequest allocated with no invariant configured")
	}
	// The check must also be inert, not panic, on a nil snapshot.
	loop.checkRequestInvariant(context.Background(), nil, nil, nil)
}

func TestStoredUnchanged(t *testing.T) {
	base := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "a", Name: "t", Arguments: `{}`}}},
		{Role: "tool", Content: "result", ToolCallID: "a"},
	}
	clone := func() []history.Message {
		out := make([]history.Message, len(base))
		copy(out, base)
		return out
	}

	if err := storedUnchanged(base, clone()); err != nil {
		t.Fatalf("identical slices reported a change: %v", err)
	}

	t.Run("rewritten content", func(t *testing.T) {
		after := clone()
		after[3].Content = "truncated"
		err := storedUnchanged(base, after)
		if err == nil || !strings.Contains(err.Error(), "content changed") {
			t.Fatalf("err = %v, want a content-changed report", err)
		}
	})

	t.Run("dropped message", func(t *testing.T) {
		if err := storedUnchanged(base, clone()[:2]); err == nil {
			t.Fatal("a shortened slice reported no change")
		}
	})

	t.Run("rewritten tool call", func(t *testing.T) {
		after := clone()
		after[2].ToolCalls = []history.ToolCall{{ID: "a", Name: "t", Arguments: `{"x":1}`}}
		if err := storedUnchanged(base, after); err == nil {
			t.Fatal("a rewritten tool call reported no change")
		}
	})
}

func TestDerivationReproduces(t *testing.T) {
	stored := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	derived := deriveRequestMessages(stored, 0).messages

	t.Run("system framing may be reshaped", func(t *testing.T) {
		sent := append([]history.Message{{Role: "system", Content: "plan mode"}}, derived...)
		sent[1].Content = "sys + memory notes"
		if err := derivationReproduces(stored, 0, sent); err != nil {
			t.Fatalf("system reshaping reported a violation: %v", err)
		}
	})

	t.Run("declared dynamic context is admitted", func(t *testing.T) {
		sent := append(append([]history.Message(nil), derived...),
			history.Message{Role: "user", Content: dynamicContextSentinel + "\nextra"})
		if err := derivationReproduces(stored, 0, sent); err != nil {
			t.Fatalf("declared injection reported a violation: %v", err)
		}
	})

	t.Run("undeclared injection is caught", func(t *testing.T) {
		sent := append(append([]history.Message(nil), derived...),
			history.Message{Role: "user", Content: "smuggled instruction"})
		if err := derivationReproduces(stored, 0, sent); err == nil {
			t.Fatal("an unlogged user message reported no violation")
		}
	})

	t.Run("rewritten conversation is caught", func(t *testing.T) {
		sent := append([]history.Message(nil), derived...)
		for i := range sent {
			if sent[i].Role == "user" {
				sent[i].Content = "tampered"
			}
		}
		if err := derivationReproduces(stored, 0, sent); err == nil {
			t.Fatal("a rewritten user message reported no violation")
		}
	})

	t.Run("dropped conversation is caught", func(t *testing.T) {
		if err := derivationReproduces(stored, 0, derived[:1]); err == nil {
			t.Fatal("a dropped message reported no violation")
		}
	})
}

// The derivation must be a pure function of its inputs: same stored history
// in, same messages out, and the caller's slice untouched. Everything the
// invariant reports rests on this.
func TestDeriveRequestMessages_IsPure(t *testing.T) {
	long := strings.Repeat("x", 40_000)
	stored := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
		{Role: "tool", Content: long, ToolCallID: "a"},
		{Role: "assistant", Content: "done"},
	}
	before := make([]history.Message, len(stored))
	copy(before, stored)

	for _, budget := range []int{0, 1, 50, 1_000_000} {
		first := deriveRequestMessages(stored, budget)
		second := deriveRequestMessages(stored, budget)

		if err := storedUnchanged(before, stored); err != nil {
			t.Fatalf("budget %d: derivation mutated its input: %v", budget, err)
		}
		if err := storedUnchanged(first.messages, second.messages); err != nil {
			t.Fatalf("budget %d: derivation is not deterministic: %v", budget, err)
		}
		if first.policy != second.policy {
			t.Fatalf("budget %d: policy %v then %v", budget, first.policy, second.policy)
		}
	}
}
