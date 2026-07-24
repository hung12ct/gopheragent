package eval

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/llm/llmfake"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"github.com/hung12ct/gopheragent/pkg/tools/toolsfake"
)

// loopFactory builds a fresh AgentLoop target per trial from the given turns.
// A new provider + in-mem session per call gives real trial isolation.
func loopFactory(turns ...llmfake.Turn) TargetFactory {
	return func(_ context.Context, _ string, _ int) (Target, error) {
		reg := tools.NewRegistry()
		reg.Register(toolsfake.NewTool("get_weather").WithResult(`{"temp_c":21}`))
		llm := &llmfake.ScriptedProvider{Turns: turns}
		loop := agent.New(history.NewInMemSessionManager(), reg, llm)
		return WrapLoop(loop), nil
	}
}

func TestRunSuiteEndToEnd(t *testing.T) {
	r := &Runner{NewTarget: loopFactory(
		llmfake.Turn{ToolCalls: []agent.PendingToolCall{{ID: "c1", Name: "get_weather", ArgsJSON: `{"city":"Tokyo"}`}}},
		llmfake.Turn{Content: "It is 21° in Tokyo."},
	)}
	suite := Suite{
		Name: "weather",
		Tasks: []Task{SingleTurn("tokyo", "weather in Tokyo?",
			Contains("Tokyo"),
			Trajectory(MatchInOrder, []ExpectedCall{{Name: "get_weather", Args: ArgsSubset(map[string]any{"city": "Tokyo"})}}),
			NoError(),
		)},
	}
	rep, err := r.RunSuite(context.Background(), suite)
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if rep.Summary.PassRate != 1 {
		t.Fatalf("pass rate = %v, want 1; task=%+v", rep.Summary.PassRate, rep.Tasks[0])
	}
	got := rep.Tasks[0].Trials[0].Turns[0].ToolCalls
	if len(got) != 1 || got[0].Result != `{"temp_c":21}` {
		t.Fatalf("tool result not captured: %+v", got)
	}
}

func TestRunSuiteMultiTurnContinuity(t *testing.T) {
	// Turn 2 must see turn 1's context on the shared session.
	r := &Runner{NewTarget: loopFactory(
		llmfake.Turn{Content: "Hello Alice."},
		llmfake.Turn{Content: "Your name is Alice."},
	)}
	suite := Suite{Name: "memory", Tasks: []Task{{
		ID: "recall-name",
		Turns: []Turn{
			{Input: history.Message{Role: "user", Content: "My name is Alice."}, Graders: []Grader{Contains("Alice")}},
			{Input: history.Message{Role: "user", Content: "What is my name?"}, Graders: []Grader{Contains("Alice")}},
		},
	}}}
	rep, err := r.RunSuite(context.Background(), suite)
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if !rep.Tasks[0].Pass {
		t.Fatalf("multi-turn task failed: %+v", rep.Tasks[0].Trials[0].Turns)
	}
	if len(rep.Tasks[0].Trials[0].Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(rep.Tasks[0].Trials[0].Turns))
	}
}

func TestRunSuitePassAtKVsAllK(t *testing.T) {
	// A flaky target: passes on odd trials, fails on even ones.
	var n atomic.Int64
	factory := func(_ context.Context, _ string, trial int) (Target, error) {
		content := "correct"
		if n.Add(1)%2 == 0 {
			content = "wrong"
		}
		reg := tools.NewRegistry()
		llm := &llmfake.ScriptedProvider{Turns: []llmfake.Turn{{Content: content}}}
		return WrapLoop(agent.New(history.NewInMemSessionManager(), reg, llm)), nil
	}
	task := func(policy PassPolicy) Task {
		tk := SingleTurn("flaky", "go", Contains("correct"))
		tk.Trials = 2
		tk.Policy = policy
		return tk
	}
	// pass@k: at least one of two trials passes.
	rep, _ := (&Runner{NewTarget: factory}).RunSuite(context.Background(), Suite{Name: "s", Tasks: []Task{task(PassPolicy{})}})
	if !rep.Tasks[0].Pass || !rep.Tasks[0].PassAtK || rep.Tasks[0].PassAllK {
		t.Fatalf("pass@k wrong: %+v", rep.Tasks[0])
	}
	// pass^k: requires all trials — should fail since one trial is wrong.
	n.Store(0)
	rep, _ = (&Runner{NewTarget: factory}).RunSuite(context.Background(), Suite{Name: "s", Tasks: []Task{task(PassPolicy{AllTrials: true})}})
	if rep.Tasks[0].Pass {
		t.Fatalf("pass^k should fail when a trial fails: %+v", rep.Tasks[0])
	}
}

func TestRunSuiteConcurrencyIsolation(t *testing.T) {
	// Each trial must get its own target; a shared counter proves the factory
	// is called once per trial and runs are independent under -race.
	var built atomic.Int64
	factory := func(_ context.Context, _ string, _ int) (Target, error) {
		built.Add(1)
		reg := tools.NewRegistry()
		llm := &llmfake.ScriptedProvider{Turns: []llmfake.Turn{{Content: "ok"}}}
		return WrapLoop(agent.New(history.NewInMemSessionManager(), reg, llm)), nil
	}
	tasks := make([]Task, 8)
	for i := range tasks {
		tk := SingleTurn(fmt.Sprintf("t%d", i), "go", Contains("ok"))
		tk.Trials = 3
		tasks[i] = tk
	}
	rep, err := (&Runner{NewTarget: factory, Concurrency: 4}).RunSuite(context.Background(), Suite{Name: "conc", Tasks: tasks})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if built.Load() != 24 {
		t.Fatalf("factory called %d times, want 24", built.Load())
	}
	if rep.Summary.PassRate != 1 {
		t.Fatalf("pass rate = %v", rep.Summary.PassRate)
	}
}

func TestRunSuiteGraderPanicRecovered(t *testing.T) {
	panicky := GraderFunc("boom", func(_ context.Context, _ *Transcript) Grade {
		panic("kaboom")
	})
	r := &Runner{NewTarget: loopFactory(llmfake.Turn{Content: "x"})}
	rep, err := r.RunSuite(context.Background(), Suite{Name: "s", Tasks: []Task{SingleTurn("p", "go", panicky)}})
	if err != nil {
		t.Fatalf("RunSuite should not error on grader panic: %v", err)
	}
	g := rep.Tasks[0].Trials[0].Turns[0].Grades[0]
	if g.Err == nil {
		t.Fatalf("expected recovered panic as Grade.Err, got %+v", g)
	}
}

func TestRunSuiteFactoryError(t *testing.T) {
	r := &Runner{NewTarget: func(_ context.Context, _ string, _ int) (Target, error) {
		return nil, fmt.Errorf("no key")
	}}
	rep, err := r.RunSuite(context.Background(), Suite{Name: "s", Tasks: []Task{SingleTurn("t", "go", Contains("x"))}})
	if err != nil {
		t.Fatalf("factory error should be per-trial, not suite error: %v", err)
	}
	if rep.Tasks[0].Pass || rep.Tasks[0].Trials[0].Error == "" {
		t.Fatalf("expected trial error recorded: %+v", rep.Tasks[0].Trials[0])
	}
}

func TestRunSuiteThreshold(t *testing.T) {
	r := &Runner{NewTarget: loopFactory(llmfake.Turn{Content: "wrong"})}
	suite := Suite{Name: "s", PassRateThreshold: 0.9, Tasks: []Task{SingleTurn("t", "go", Contains("right"))}}
	rep, _ := r.RunSuite(context.Background(), suite)
	if !rep.Summary.BelowThreshold {
		t.Fatalf("expected BelowThreshold with 0%% pass and 0.9 threshold")
	}
}

func TestRunSuiteErrors(t *testing.T) {
	if _, err := (&Runner{}).RunSuite(context.Background(), Suite{Name: "s", Tasks: []Task{SingleTurn("t", "go")}}); err == nil {
		t.Fatalf("expected error for nil factory")
	}
	if _, err := (&Runner{NewTarget: loopFactory()}).RunSuite(context.Background(), Suite{Name: "empty"}); err == nil {
		t.Fatalf("expected error for empty suite")
	}
}
