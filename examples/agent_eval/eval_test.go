// Package agenteval is a runnable example of the pkg/eval harness. The test
// here is the "inside go test" consumption mode: fully deterministic via
// llmfake, so it runs in CI with no API keys.
package agenteval

import (
	"context"
	"os"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/eval"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/llm/llmfake"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"github.com/hung12ct/gopheragent/pkg/tools/toolsfake"
)

// newTarget builds a fresh, deterministic weather agent per trial: a scripted
// provider that calls get_weather then answers, plus a fake tool.
func newTarget(_ context.Context, _ string, _ int) (eval.Target, error) {
	reg := tools.NewRegistry()
	reg.Register(toolsfake.NewTool("get_weather").WithResult(`{"temp_c":21}`))
	llm := &llmfake.ScriptedProvider{Turns: []llmfake.Turn{
		{ToolCalls: []agent.PendingToolCall{{ID: "c1", Name: "get_weather", ArgsJSON: `{"city":"Tokyo"}`}}},
		{Content: "It is 21° in Tokyo right now."},
	}}
	return eval.WrapLoop(agent.New(history.NewInMemSessionManager(), reg, llm)), nil
}

func TestExampleSuite(t *testing.T) {
	suite := eval.Suite{
		Name:              "weather-smoke",
		PassRateThreshold: 1.0,
		Tasks: []eval.Task{
			eval.SingleTurn("get-weather-tokyo", "What's the weather in Tokyo?",
				eval.Contains("Tokyo"),
				eval.Regexp(`\d+°`),
				eval.Trajectory(eval.MatchInOrder, []eval.ExpectedCall{
					{Name: "get_weather", Args: eval.ArgsSubset(map[string]any{"city": "Tokyo"})},
				}),
				eval.NoError(),
			),
		},
	}
	rep := eval.RunT(t, &eval.Runner{NewTarget: newTarget}, suite)
	if rep.Summary.BelowThreshold {
		t.Fatalf("suite regressed: pass rate %.0f%%", rep.Summary.PassRate*100)
	}
}

// TestSuiteYAMLLoads verifies the shipped suite.yaml stays valid. A fake
// provider satisfies the judge block without needing a key.
func TestSuiteYAMLLoads(t *testing.T) {
	if _, err := os.Stat("suite.yaml"); err != nil {
		t.Skip("suite.yaml not present")
	}
	if _, err := eval.LoadSuite("suite.yaml", &llmfake.ScriptedProvider{}); err != nil {
		t.Fatalf("suite.yaml failed to load: %v", err)
	}
}
