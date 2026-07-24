package eval

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Grade is one grader's verdict on one Transcript.
type Grade struct {
	Grader string  `json:"grader"`
	Pass   bool    `json:"pass"`
	Score  float64 `json:"score"`            // 0..1; code graders emit 0 or 1
	Reason string  `json:"reason,omitempty"` // human-readable why (esp. on failure)
	// Unknown marks a verdict the grader could not determine (a judge escape
	// hatch). Unknown grades are excluded from a trial's pass/fail decision.
	Unknown bool `json:"unknown,omitempty"`
	// Err is a grader infrastructure failure (LLM call failed, panic) — not a
	// task failure. Reported distinctly; excluded from pass/fail like Unknown.
	Err error `json:"-"`
}

// Grader scores one Transcript. Implementations must be safe for concurrent
// use: the runner may grade trials in parallel.
type Grader interface {
	Name() string
	Grade(ctx context.Context, tr *Transcript) Grade
}

// graderFunc adapts a closure to Grader.
type graderFunc struct {
	name string
	fn   func(ctx context.Context, tr *Transcript) Grade
}

func (g graderFunc) Name() string { return g.name }
func (g graderFunc) Grade(ctx context.Context, tr *Transcript) Grade {
	grade := g.fn(ctx, tr)
	if grade.Grader == "" {
		grade.Grader = g.name
	}
	return grade
}

// GraderFunc adapts a closure into a Grader. name labels the grade in reports.
func GraderFunc(name string, fn func(ctx context.Context, tr *Transcript) Grade) Grader {
	return graderFunc{name: name, fn: fn}
}

// pass builds a passing Grade; fail builds a failing one, both scored 1/0.
func pass(name string) Grade { return Grade{Grader: name, Pass: true, Score: 1} }
func fail(name, reason string) Grade {
	return Grade{Grader: name, Pass: false, Score: 0, Reason: reason}
}

// Contains passes when FinalAnswer contains every substring (case-sensitive).
func Contains(substrs ...string) Grader {
	const name = "contains"
	return GraderFunc(name, func(_ context.Context, tr *Transcript) Grade {
		for _, s := range substrs {
			if !strings.Contains(tr.FinalAnswer, s) {
				return fail(name, fmt.Sprintf("answer missing %q", s))
			}
		}
		return pass(name)
	})
}

// ContainsFold is Contains with case-insensitive matching.
func ContainsFold(substrs ...string) Grader {
	const name = "contains_fold"
	return GraderFunc(name, func(_ context.Context, tr *Transcript) Grade {
		lower := strings.ToLower(tr.FinalAnswer)
		for _, s := range substrs {
			if !strings.Contains(lower, strings.ToLower(s)) {
				return fail(name, fmt.Sprintf("answer missing %q (fold)", s))
			}
		}
		return pass(name)
	})
}

// Regexp passes when FinalAnswer matches pattern. It compiles once at
// construction and panics on a bad pattern (like regexp.MustCompile), so
// misconfiguration fails loudly at wiring time, not mid-suite.
func Regexp(pattern string) Grader {
	const name = "regexp"
	re := regexp.MustCompile(pattern)
	return GraderFunc(name, func(_ context.Context, tr *Transcript) Grade {
		if re.MatchString(tr.FinalAnswer) {
			return pass(name)
		}
		return fail(name, fmt.Sprintf("answer does not match /%s/", pattern))
	})
}

// Exact passes when FinalAnswer equals want after trimming surrounding space.
func Exact(want string) Grader {
	const name = "exact"
	want = strings.TrimSpace(want)
	return GraderFunc(name, func(_ context.Context, tr *Transcript) Grade {
		if strings.TrimSpace(tr.FinalAnswer) == want {
			return pass(name)
		}
		return fail(name, fmt.Sprintf("answer != %q", want))
	})
}

// NoError passes when the run finished cleanly (no error, terminated "done").
func NoError() Grader {
	const name = "no_error"
	return GraderFunc(name, func(_ context.Context, tr *Transcript) Grade {
		if tr.Err == nil && tr.TerminatedBy == "done" {
			return pass(name)
		}
		reason := "terminated " + tr.TerminatedBy
		if tr.Err != nil {
			reason = tr.Err.Error()
		}
		return fail(name, reason)
	})
}

// All passes only when every sub-grader passes; Score is the minimum.
func All(gs ...Grader) Grader {
	const name = "all"
	return GraderFunc(name, func(ctx context.Context, tr *Transcript) Grade {
		out := pass(name)
		for _, g := range gs {
			sub := g.Grade(ctx, tr)
			if sub.Score < out.Score {
				out.Score = sub.Score
			}
			if !sub.Pass && !sub.Unknown {
				out.Pass = false
				out.Reason = g.Name() + ": " + sub.Reason
				return out
			}
		}
		return out
	})
}

// Any passes when at least one sub-grader passes; Score is the maximum.
func Any(gs ...Grader) Grader {
	const name = "any"
	return GraderFunc(name, func(ctx context.Context, tr *Transcript) Grade {
		out := Grade{Grader: name}
		var reasons []string
		for _, g := range gs {
			sub := g.Grade(ctx, tr)
			if sub.Score > out.Score {
				out.Score = sub.Score
			}
			if sub.Pass {
				out.Pass = true
				return out
			}
			reasons = append(reasons, g.Name()+": "+sub.Reason)
		}
		out.Reason = "none passed [" + strings.Join(reasons, "; ") + "]"
		return out
	})
}
