package eval

import (
	"context"
	"testing"
)

// RunT runs a suite as Go subtests — one t.Run per task. A failing task
// reports its failing grades (and any harness/grader errors) via t.Errorf, so
// `go test` output points straight at what broke. It returns the full report
// so a test can additionally assert on usage, cost, or pass rate.
//
// With a Target backed by llmfake.ScriptedProvider this is fully
// deterministic and needs no API keys — the shape the library's own CI uses.
func RunT(t *testing.T, r *Runner, s Suite) *SuiteReport {
	t.Helper()
	rep, err := r.RunSuite(context.Background(), s)
	if err != nil {
		t.Fatalf("eval: RunSuite: %v", err)
	}
	for _, task := range rep.Tasks {
		t.Run(task.TaskID, func(t *testing.T) {
			if task.Pass {
				return
			}
			reportTaskFailure(t, task)
		})
	}
	return rep
}

// reportTaskFailure emits a t.Errorf line per failing grade / harness error.
func reportTaskFailure(t *testing.T, task TaskReport) {
	t.Helper()
	for _, trial := range task.Trials {
		if trial.Pass {
			continue
		}
		if trial.Error != "" {
			t.Errorf("trial %d: %s", trial.Trial, trial.Error)
		}
		for _, turn := range trial.Turns {
			for _, g := range turn.Grades {
				switch {
				case g.Err != nil:
					t.Errorf("trial %d turn %d: grader %s errored: %v", trial.Trial, turn.TurnIndex, g.Grader, g.Err)
				case !g.Pass && !g.Unknown:
					t.Errorf("trial %d turn %d: %s failed: %s", trial.Trial, turn.TurnIndex, g.Grader, g.Reason)
				}
			}
		}
	}
}
