package eval

import (
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// Turn is one exchange in a Task's conversation: a user input and the graders
// applied to that turn's Transcript. Multi-turn tasks share one session
// within a trial, so later turns can test memory and anaphora.
type Turn struct {
	Input   history.Message
	Graders []Grader
}

// Task is one test case — a conversation of one or more turns.
type Task struct {
	// ID must be unique within a Suite; it names the JUnit testcase.
	ID string
	// Turns is the conversation; at least one is required.
	Turns []Turn
	// Trials overrides Suite.Trials for this task (0 = inherit).
	Trials int
	// Timeout bounds one whole trial (all turns). 0 = inherit Suite.Timeout.
	Timeout time.Duration
	// Policy decides pass/fail across graders and trials.
	Policy PassPolicy
}

// PassPolicy decides how grades and trials combine into pass/fail.
type PassPolicy struct {
	// MinScore: a trial passes when the mean of its non-excluded grade scores
	// is >= MinScore. When 0 (the default), a trial passes only if every
	// non-Unknown, non-error grade has Pass=true.
	MinScore float64
	// AllTrials selects pass^k: the task passes only if every trial passes.
	// Default (false) is pass@k: at least one passing trial passes the task.
	AllTrials bool
}

// Suite is a named collection of tasks with shared defaults.
type Suite struct {
	Name  string
	Tasks []Task
	// Trials is the default per-task trial count (min 1 applied at run time).
	Trials int
	// Timeout is the default per-trial timeout (0 = none).
	Timeout time.Duration
	// Concurrency caps parallel trials across the whole suite (min 1).
	Concurrency int
	// PassRateThreshold in [0,1]: RunSuite marks Summary.BelowThreshold when
	// the task pass rate falls under it. 0 disables the check.
	PassRateThreshold float64
}

// SingleTurn builds a one-turn Task from a plain-text input — the common case.
func SingleTurn(id, input string, graders ...Grader) Task {
	return Task{
		ID:    id,
		Turns: []Turn{{Input: history.Message{Role: "user", Content: input}, Graders: graders}},
	}
}
