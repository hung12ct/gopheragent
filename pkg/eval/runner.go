package eval

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// Runner executes a Suite against targets built by NewTarget.
type Runner struct {
	// NewTarget builds a fresh Target per (task, trial). Required.
	NewTarget TargetFactory
	// Concurrency overrides Suite.Concurrency when > 0.
	Concurrency int
	// Capture tunes transcript capture (KeepEvents, IncludeSubagents).
	Capture CaptureOptions
	// OnTrial, when non-nil, observes each finished trial (progress logging).
	// It is called from worker goroutines and must be concurrency-safe.
	OnTrial func(TrialReport)
	// OnTranscript, when non-nil, receives each turn's raw Transcript as it is
	// captured, before grading. Called from worker goroutines and must be
	// concurrency-safe. Use it to persist transcripts for later re-grading
	// (see WriteTranscripts / GradeRecorded).
	OnTranscript func(*Transcript)
}

// RunSuite executes every (task, trial) pair with bounded concurrency and
// returns the aggregated report. The returned error covers harness failures
// only (nil factory, empty suite); task and grader failures live in the
// report. Grader panics are recovered into Grade.Err. A cancelled ctx leaves
// later trials marked ctx-terminated but still emits a partial report.
func (r *Runner) RunSuite(ctx context.Context, s Suite) (*SuiteReport, error) {
	if r.NewTarget == nil {
		return nil, fmt.Errorf("eval: RunSuite: NewTarget factory is nil")
	}
	if len(s.Tasks) == 0 {
		return nil, fmt.Errorf("eval: RunSuite: suite %q has no tasks", s.Name)
	}
	start := time.Now()
	results := make([][]TrialReport, len(s.Tasks))
	for ti := range s.Tasks {
		results[ti] = make([]TrialReport, effectiveTrials(s.Tasks[ti], s))
	}
	r.dispatch(ctx, s, results)
	rep := &SuiteReport{Suite: s.Name, StartedAt: start, Tasks: make([]TaskReport, len(s.Tasks))}
	for ti := range s.Tasks {
		rep.Tasks[ti] = aggregateTask(s.Tasks[ti], results[ti])
	}
	rep.Summary = summarize(rep.Tasks, s.PassRateThreshold, time.Since(start))
	return rep, nil
}

// dispatch runs all trials with a bounded worker pool, writing each result to
// its pre-allocated slot (distinct indices → no shared-state mutation).
func (r *Runner) dispatch(ctx context.Context, s Suite, results [][]TrialReport) {
	conc := r.Concurrency
	if conc <= 0 {
		conc = s.Concurrency
	}
	if conc < 1 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for ti := range s.Tasks {
		for tri := range results[ti] {
			wg.Add(1)
			sem <- struct{}{}
			go func(ti, tri int) {
				defer wg.Done()
				defer func() { <-sem }()
				tr := r.runTrial(ctx, s, s.Tasks[ti], tri+1)
				results[ti][tri] = tr
				if r.OnTrial != nil {
					r.OnTrial(tr)
				}
			}(ti, tri)
		}
	}
	wg.Wait()
}

// runTrial executes one trial: build a fresh target, run every turn on a
// shared session, grade each turn, and combine into a TrialReport.
func (r *Runner) runTrial(ctx context.Context, s Suite, task Task, trial int) TrialReport {
	out := TrialReport{TaskID: task.ID, Trial: trial, Pass: true, TerminatedBy: "done"}
	trialCtx := ctx
	if to := effectiveTimeout(task, s); to > 0 {
		var cancel context.CancelFunc
		trialCtx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}
	target, err := r.NewTarget(trialCtx, task.ID, trial)
	if err != nil {
		out.Pass = false
		out.Error = fmt.Sprintf("factory: %v", err)
		return out
	}
	sessionKey := fmt.Sprintf("eval/%s/%s/trial-%d", s.Name, task.ID, trial)
	for i, turn := range task.Turns {
		msg := turn.Input
		if msg.Role == "" {
			msg.Role = "user"
		}
		tr := Capture(trialCtx, target, sessionKey, msg, r.Capture)
		tr.TaskID, tr.Trial, tr.TurnIndex = task.ID, trial, i
		if r.OnTranscript != nil {
			r.OnTranscript(tr)
		}
		turnRep := gradeTurn(trialCtx, i, msg, tr, turn.Graders, task.Policy)
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

// gradeTurn runs a turn's graders (panic-safe) and applies the pass policy.
func gradeTurn(ctx context.Context, idx int, msg history.Message, tr *Transcript, graders []Grader, policy PassPolicy) TurnReport {
	rep := TurnReport{TurnIndex: idx, Input: msg.Content, FinalAnswer: tr.FinalAnswer, ToolCalls: tr.ToolCalls}
	for _, g := range graders {
		rep.Grades = append(rep.Grades, safeGrade(ctx, g, tr))
	}
	rep.Pass = gradesPass(rep.Grades, policy)
	return rep
}

// safeGrade calls a grader, converting a panic into a Grade carrying Err.
func safeGrade(ctx context.Context, g Grader, tr *Transcript) (grade Grade) {
	defer func() {
		if rec := recover(); rec != nil {
			grade = Grade{Grader: g.Name(), Err: fmt.Errorf("eval: grader %s panicked: %v", g.Name(), rec)}
		}
	}()
	return g.Grade(ctx, tr)
}

// gradesPass applies a PassPolicy to one turn's grades. Unknown and errored
// grades are excluded from the decision (Anthropic's "unknown" escape hatch);
// a turn with no decisive grades passes vacuously.
func gradesPass(grades []Grade, policy PassPolicy) bool {
	var scored []Grade
	for _, g := range grades {
		if g.Unknown || g.Err != nil {
			continue
		}
		scored = append(scored, g)
	}
	if len(scored) == 0 {
		return true
	}
	if policy.MinScore > 0 {
		var sum float64
		for _, g := range scored {
			sum += g.Score
		}
		return sum/float64(len(scored)) >= policy.MinScore
	}
	for _, g := range scored {
		if !g.Pass {
			return false
		}
	}
	return true
}

// aggregateTask folds a task's trials into a TaskReport, computing pass@k /
// pass^k and the policy-selected Pass.
func aggregateTask(task Task, trials []TrialReport) TaskReport {
	rep := TaskReport{TaskID: task.ID, Trials: trials, PassAllK: len(trials) > 0}
	for _, tr := range trials {
		if tr.Pass {
			rep.PassAtK = true
		} else {
			rep.PassAllK = false
		}
	}
	if task.Policy.AllTrials {
		rep.Pass = rep.PassAllK
	} else {
		rep.Pass = rep.PassAtK
	}
	return rep
}

// summarize rolls up task reports into a Summary.
func summarize(tasks []TaskReport, threshold float64, dur time.Duration) Summary {
	sum := Summary{Tasks: len(tasks), Duration: dur}
	for _, t := range tasks {
		if t.Pass {
			sum.Passed++
		} else {
			sum.Failed++
		}
		for _, tr := range t.Trials {
			sum.TotalUsage.PromptTokens += tr.Usage.PromptTokens
			sum.TotalUsage.CompletionTokens += tr.Usage.CompletionTokens
			sum.TotalUsage.TotalTokens += tr.Usage.TotalTokens
			sum.TotalCostUSD += tr.CostUSD
		}
	}
	if sum.Tasks > 0 {
		sum.PassRate = float64(sum.Passed) / float64(sum.Tasks)
	}
	if threshold > 0 && sum.PassRate < threshold {
		sum.BelowThreshold = true
	}
	return sum
}

// effectiveTrials resolves a task's trial count under a suite (min 1).
func effectiveTrials(t Task, s Suite) int {
	n := t.Trials
	if n == 0 {
		n = s.Trials
	}
	if n < 1 {
		return 1
	}
	return n
}

// effectiveTimeout resolves a task's per-trial timeout under a suite.
func effectiveTimeout(t Task, s Suite) time.Duration {
	if t.Timeout != 0 {
		return t.Timeout
	}
	return s.Timeout
}
