package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// WriteTranscripts writes transcripts as JSONL (one JSON object per line),
// streaming through a buffered writer so memory stays flat regardless of
// count. Persist a run's transcripts to re-grade later without re-running the
// agent (see GradeRecorded).
func WriteTranscripts(w io.Writer, ts []*Transcript) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for _, tr := range ts {
		if err := enc.Encode(tr); err != nil {
			return fmt.Errorf("eval: write transcripts: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("eval: write transcripts: %w", err)
	}
	return nil
}

// ReadTranscripts reads JSONL transcripts written by WriteTranscripts. It
// reconstructs Transcript.Err from the serialized ErrMessage so replay graders
// (NoError, Judge) behave exactly as they did live.
func ReadTranscripts(r io.Reader) ([]*Transcript, error) {
	var out []*Transcript
	dec := json.NewDecoder(r)
	for {
		var tr Transcript
		if err := dec.Decode(&tr); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("eval: read transcripts: %w", err)
		}
		if tr.ErrMessage != "" {
			tr.Err = errors.New(tr.ErrMessage)
		}
		out = append(out, &tr)
	}
	return out, nil
}

// GradeRecorded re-runs a suite's graders against already-captured transcripts
// without invoking the agent — cheap iteration on a revised judge rubric or a
// tightened trajectory. Transcripts are matched to tasks by TaskID and to
// turns by TurnIndex; a transcript for a task not in the suite is ignored.
func GradeRecorded(ctx context.Context, s Suite, ts []*Transcript) (*SuiteReport, error) {
	if len(s.Tasks) == 0 {
		return nil, fmt.Errorf("eval: GradeRecorded: suite %q has no tasks", s.Name)
	}
	start := time.Now()
	byTask := indexTranscripts(ts)
	rep := &SuiteReport{Suite: s.Name, StartedAt: start, Tasks: make([]TaskReport, len(s.Tasks))}
	for ti, task := range s.Tasks {
		rep.Tasks[ti] = regradeTask(ctx, task, byTask[task.ID])
	}
	rep.Summary = summarize(rep.Tasks, s.PassRateThreshold, time.Since(start))
	return rep, nil
}

// indexTranscripts groups transcripts by task ID then trial number.
func indexTranscripts(ts []*Transcript) map[string]map[int][]*Transcript {
	out := make(map[string]map[int][]*Transcript)
	for _, tr := range ts {
		byTrial := out[tr.TaskID]
		if byTrial == nil {
			byTrial = make(map[int][]*Transcript)
			out[tr.TaskID] = byTrial
		}
		byTrial[tr.Trial] = append(byTrial[tr.Trial], tr)
	}
	return out
}

// regradeTask rebuilds a TaskReport from stored transcripts, in ascending
// trial order for deterministic output.
func regradeTask(ctx context.Context, task Task, byTrial map[int][]*Transcript) TaskReport {
	trialNums := make([]int, 0, len(byTrial))
	for n := range byTrial {
		trialNums = append(trialNums, n)
	}
	sort.Ints(trialNums)
	trials := make([]TrialReport, 0, len(trialNums))
	for _, n := range trialNums {
		trials = append(trials, regradeTrial(ctx, task, n, byTrial[n]))
	}
	return aggregateTask(task, trials)
}

// regradeTrial re-grades one trial's turns from stored transcripts.
func regradeTrial(ctx context.Context, task Task, trialNum int, turns []*Transcript) TrialReport {
	sort.Slice(turns, func(i, j int) bool { return turns[i].TurnIndex < turns[j].TurnIndex })
	out := TrialReport{TaskID: task.ID, Trial: trialNum, Pass: true, TerminatedBy: "done"}
	for _, tr := range turns {
		var graders []Grader
		if tr.TurnIndex < len(task.Turns) {
			graders = task.Turns[tr.TurnIndex].Graders
		}
		turnRep := gradeTurn(ctx, tr.TurnIndex, tr.Input, tr, graders, task.Policy)
		out.Turns = append(out.Turns, turnRep)
		out.Usage.PromptTokens += tr.Usage.PromptTokens
		out.Usage.CompletionTokens += tr.Usage.CompletionTokens
		out.Usage.TotalTokens += tr.Usage.TotalTokens
		out.CostUSD += tr.CostUSD
		out.Latency += tr.Latency
		out.TerminatedBy = tr.TerminatedBy
		if !turnRep.Pass {
			out.Pass = false
		}
	}
	return out
}

// TaskFromHistory builds a golden Task from a recorded session: each user
// message opens a turn, the assistant tool calls become an in-order trajectory
// expectation, and the final assistant text becomes a Contains grader. The
// result is a starting scaffold — tighten or relax the generated graders by
// hand. This is the cheapest way to seed a suite from a real conversation.
func TaskFromHistory(id string, msgs []history.Message) Task {
	task := Task{ID: id}
	var cur *Turn
	var calls []ExpectedCall
	var answer string
	flush := func() {
		if cur == nil {
			return
		}
		var graders []Grader
		if len(calls) > 0 {
			graders = append(graders, Trajectory(MatchInOrder, calls))
		}
		if a := strings.TrimSpace(answer); a != "" {
			graders = append(graders, Contains(a))
		}
		cur.Graders = graders
		task.Turns = append(task.Turns, *cur)
		cur, calls, answer = nil, nil, ""
	}
	for _, m := range msgs {
		switch m.Role {
		case "user":
			flush()
			turn := Turn{Input: m}
			cur = &turn
		case "assistant":
			for _, tc := range m.ToolCalls {
				calls = append(calls, ExpectedCall{Name: tc.Name, Args: ArgsExact(tc.Arguments)})
			}
			if m.Content != "" {
				answer = m.Content
			}
		}
	}
	flush()
	return task
}
