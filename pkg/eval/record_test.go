package eval

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
)

func TestTranscriptsRoundTrip(t *testing.T) {
	ts := []*Transcript{
		{TaskID: "a", Trial: 1, TurnIndex: 0, FinalAnswer: "hello", TerminatedBy: "done",
			ToolCalls: []ToolCallRecord{{ID: "c1", Name: "t", ArgsJSON: `{}`, Result: "r"}}},
		{TaskID: "a", Trial: 1, TurnIndex: 1, FinalAnswer: "bye", TerminatedBy: "error", ErrMessage: "boom"},
	}
	var buf bytes.Buffer
	if err := WriteTranscripts(&buf, ts); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := ReadTranscripts(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(back) != 2 || back[0].FinalAnswer != "hello" || back[0].ToolCalls[0].Result != "r" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
	if back[1].Err == nil || back[1].Err.Error() != "boom" {
		t.Fatalf("Err not reconstructed from ErrMessage: %+v", back[1])
	}
}

func TestGradeRecordedMatchesLive(t *testing.T) {
	// Re-grading stored transcripts should reproduce a live verdict.
	ts := []*Transcript{
		{TaskID: "greet", Trial: 1, TurnIndex: 0, Input: history.Message{Content: "hi"}, FinalAnswer: "hello Alice", TerminatedBy: "done", Latency: time.Second},
	}
	suite := Suite{Name: "s", Tasks: []Task{SingleTurn("greet", "hi", Contains("Alice"))}}
	rep, err := GradeRecorded(context.Background(), suite, ts)
	if err != nil {
		t.Fatalf("GradeRecorded: %v", err)
	}
	if !rep.Tasks[0].Pass {
		t.Fatalf("expected pass: %+v", rep.Tasks[0])
	}
	// A tightened grader should now fail against the same transcript.
	suite.Tasks[0] = SingleTurn("greet", "hi", Contains("Bob"))
	rep, _ = GradeRecorded(context.Background(), suite, ts)
	if rep.Tasks[0].Pass {
		t.Fatalf("tightened grader should fail on replay")
	}
}

func TestGradeRecordedMultiTurn(t *testing.T) {
	ts := []*Transcript{
		{TaskID: "conv", Trial: 1, TurnIndex: 1, FinalAnswer: "second", TerminatedBy: "done"},
		{TaskID: "conv", Trial: 1, TurnIndex: 0, FinalAnswer: "first", TerminatedBy: "done"},
	}
	suite := Suite{Name: "s", Tasks: []Task{{ID: "conv", Turns: []Turn{
		{Graders: []Grader{Contains("first")}},
		{Graders: []Grader{Contains("second")}},
	}}}}
	rep, err := GradeRecorded(context.Background(), suite, ts)
	if err != nil {
		t.Fatalf("GradeRecorded: %v", err)
	}
	if !rep.Tasks[0].Pass || len(rep.Tasks[0].Trials[0].Turns) != 2 {
		t.Fatalf("multi-turn regrade wrong: %+v", rep.Tasks[0])
	}
}

func TestTaskFromHistory(t *testing.T) {
	msgs := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "weather in Tokyo?"},
		{Role: "assistant", ToolCalls: []history.ToolCall{{Name: "get_weather", Arguments: `{"city":"Tokyo"}`}}},
		{Role: "tool", Content: `{"temp_c":21}`},
		{Role: "assistant", Content: "It is 21° in Tokyo."},
	}
	task := TaskFromHistory("golden", msgs)
	if len(task.Turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(task.Turns))
	}
	if len(task.Turns[0].Graders) != 2 {
		t.Fatalf("expected trajectory + contains graders, got %d", len(task.Turns[0].Graders))
	}
	// The generated graders should pass against a transcript matching the source.
	tr := &Transcript{
		FinalAnswer: "It is 21° in Tokyo.",
		ToolCalls:   []ToolCallRecord{{Name: "get_weather", ArgsJSON: `{"city":"Tokyo"}`}},
	}
	for _, g := range task.Turns[0].Graders {
		if grade := g.Grade(context.Background(), tr); !grade.Pass {
			t.Fatalf("generated grader %s failed on source transcript: %s", g.Name(), grade.Reason)
		}
	}
}
