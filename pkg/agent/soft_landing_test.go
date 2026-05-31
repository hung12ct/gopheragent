package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func TestWithSoftLandingHint_AppendsOnFinalTwoIterations(t *testing.T) {
	base := []history.Message{{Role: "user", Content: "hi"}}
	cases := []struct {
		iter, max int
		want      bool
	}{
		{0, 5, false},
		{2, 5, false},
		{3, 5, true},
		{4, 5, true},
	}
	for _, c := range cases {
		got := withSoftLandingHint(c.iter, c.max, base)
		appended := len(got) > len(base)
		if appended != c.want {
			t.Fatalf("iter=%d max=%d: appended=%v, want %v", c.iter, c.max, appended, c.want)
		}
	}
}

func TestWithSoftLandingHint_Idempotent(t *testing.T) {
	msgs := []history.Message{{Role: "user", Content: "hi"}}
	once := withSoftLandingHint(4, 5, msgs)
	twice := withSoftLandingHint(4, 5, once)
	if len(twice) != len(once) {
		t.Fatalf("expected idempotent, second call appended again: len=%d", len(twice))
	}
}

func TestWithSoftLandingHint_DoesNotMutateInput(t *testing.T) {
	msgs := []history.Message{{Role: "user", Content: "hi"}}
	_ = withSoftLandingHint(4, 5, msgs)
	if len(msgs) != 1 {
		t.Fatalf("input slice was mutated: len=%d", len(msgs))
	}
}

// TestSoftLanding_NotPersistedToHistory pins the regression: prior to the
// fix, the soft-landing system message was append-and-SetHistory'd, so it
// accumulated in saved history across user turns. This test runs an agent
// to its iteration limit and asserts saved history contains zero copies of
// the soft-landing sentinel.
func TestSoftLanding_NotPersistedToHistory(t *testing.T) {
	// Provider produces tool calls forever — the agent will hit MaxIters
	// and the soft-landing nudge will fire on the final two iterations.
	provider := &toolEverProvider{name: "noop"}

	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	reg.Register(&noopTool{name: "noop"})
	loop := NewAgentLoop(sm, reg, provider)
	loop.MaxIters = 4

	_, _ = loop.RunIteration(context.Background(), "s1", "go")

	hist, _ := sm.History(context.Background(), "s1")
	for _, m := range hist {
		if strings.Contains(m.Content, softLandingSentinel) {
			t.Fatalf("soft-landing sentinel persisted to saved history: %+v", m)
		}
	}
}

type toolEverProvider struct {
	name  string
	calls int
}

func (p *toolEverProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- StreamEvent) (LLMResult, error) {
	p.calls++
	return LLMResult{ToolCalls: []PendingToolCall{{ID: p.idStr(), Name: p.name, ArgsJSON: `{}`}}}, nil
}

func (p *toolEverProvider) idStr() string {
	const digits = "0123456789"
	if p.calls < 10 {
		return "c" + string(digits[p.calls])
	}
	return "cN"
}

type noopTool struct{ name string }

func (t *noopTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        t.name,
		Description: "no-op",
		Display:     tools.DefaultDisplay(t.name, "no-op"),
	}
}

func (t *noopTool) Execute(_ context.Context, _ string) (tools.Result, error) {
	return tools.Text(`"ok"`), nil
}
