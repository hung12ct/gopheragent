package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/llm/llmfake"
)

const validSuiteYAML = `
suite:
  name: weather
  trials: 2
  concurrency: 2
  timeout: 30s
  pass_rate_threshold: 0.8
  tasks:
    - id: tokyo
      pass: { all_trials: true }
      input: "weather in Tokyo?"
      expect:
        tools:
          mode: in_order
          calls:
            - name: get_weather
              args_subset: { city: Tokyo }
        answer:
          contains: ["Tokyo"]
          regex: '\d+'
        no_error: true
    - id: multi
      turns:
        - input: "hi"
          expect:
            answer: { contains: ["hello"] }
        - input: "bye"
          expect:
            answer: { contains: ["goodbye"] }
`

func TestLoadSuiteValid(t *testing.T) {
	s, err := ParseSuiteBytes("test.yaml", []byte(validSuiteYAML), nil)
	if err != nil {
		t.Fatalf("ParseSuiteBytes: %v", err)
	}
	if s.Name != "weather" || s.Trials != 2 || s.Concurrency != 2 || s.PassRateThreshold != 0.8 {
		t.Fatalf("suite fields wrong: %+v", s)
	}
	if s.Timeout.String() != "30s" {
		t.Fatalf("timeout = %v", s.Timeout)
	}
	if len(s.Tasks) != 2 {
		t.Fatalf("expected 2 tasks")
	}
	if !s.Tasks[0].Policy.AllTrials {
		t.Fatalf("pass.all_trials not compiled")
	}
	// tokyo: trajectory + contains + regex + no_error = 4 graders.
	if got := len(s.Tasks[0].Turns[0].Graders); got != 4 {
		t.Fatalf("tokyo graders = %d, want 4", got)
	}
	if len(s.Tasks[1].Turns) != 2 {
		t.Fatalf("multi task should have 2 turns")
	}
}

func TestLoadSuiteCompiledGradersWork(t *testing.T) {
	s, err := ParseSuiteBytes("test.yaml", []byte(validSuiteYAML), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tr := &Transcript{
		FinalAnswer:  "It is 21 in Tokyo.",
		TerminatedBy: "done",
		ToolCalls:    []ToolCallRecord{{Name: "get_weather", ArgsJSON: `{"city":"Tokyo"}`}},
	}
	for _, g := range s.Tasks[0].Turns[0].Graders {
		if grade := g.Grade(context.Background(), tr); !grade.Pass {
			t.Fatalf("compiled grader %s failed: %s", g.Name(), grade.Reason)
		}
	}
}

func TestLoadSuiteValidationAggregates(t *testing.T) {
	bad := `
suite:
  name: ""
  pass_rate_threshold: 2
  timeout: "notaduration"
  tasks:
    - id: ""
      input: "x"
      expect:
        tools: { mode: sideways, calls: [{ name: "" }] }
        answer: { regex: "[" }
    - id: dup
      input: "y"
      expect: { answer: { contains: ["z"] } }
    - id: dup
      input: "w"
      expect: { answer: { contains: ["q"] } }
`
	_, err := ParseSuiteBytes("bad.yaml", []byte(bad), nil)
	if err == nil {
		t.Fatalf("expected validation error")
	}
	ve, ok := err.(*SuiteValidationError)
	if !ok {
		t.Fatalf("wrong error type: %T", err)
	}
	joined := strings.Join(ve.Issues, "\n")
	for _, want := range []string{"suite.name", "pass_rate_threshold", "timeout", ".id is required", "mode", "does not compile", "duplicated"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing issue %q in:\n%s", want, joined)
		}
	}
}

func TestLoadSuiteJudgeNeedsProvider(t *testing.T) {
	y := `
suite:
  name: s
  tasks:
    - id: j
      input: "x"
      expect:
        judge: { rubric: "must be good" }
`
	if _, err := ParseSuiteBytes("j.yaml", []byte(y), nil); err == nil {
		t.Fatalf("judge without provider should fail validation")
	}
	s, err := ParseSuiteBytes("j.yaml", []byte(y), &llmfake.ScriptedProvider{})
	if err != nil {
		t.Fatalf("judge with provider should compile: %v", err)
	}
	if len(s.Tasks[0].Turns[0].Graders) != 1 {
		t.Fatalf("expected a judge grader")
	}
}

func TestLoadSuiteShorthandXorTurns(t *testing.T) {
	y := `
suite:
  name: s
  tasks:
    - id: both
      input: "x"
      turns:
        - input: "y"
`
	if _, err := ParseSuiteBytes("b.yaml", []byte(y), nil); err == nil {
		t.Fatalf("input+turns together should fail validation")
	}
}
