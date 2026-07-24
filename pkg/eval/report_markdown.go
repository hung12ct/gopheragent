package eval

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// WriteMarkdown writes a PR-comment-friendly summary: a per-task table plus
// collapsible failure details.
func WriteMarkdown(w io.Writer, rep *SuiteReport) error {
	var b strings.Builder
	s := rep.Summary
	fmt.Fprintf(&b, "## Eval: %s — %d/%d passed (%.1f%%)\n\n", rep.Suite, s.Passed, s.Tasks, s.PassRate*100)
	b.WriteString("| Task | Result | pass@k | pass^k | Tokens | Cost | Latency (avg) |\n")
	b.WriteString("|------|--------|--------|--------|-------:|-----:|--------------:|\n")
	for _, task := range rep.Tasks {
		toks, cost, lat := taskAverages(task)
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | $%.4f | %s |\n",
			task.TaskID, taskLabel(task), yesNo(task.PassAtK), yesNo(task.PassAllK),
			toks, cost, lat.Round(time.Millisecond))
	}
	writeFailureSections(&b, rep)
	fmt.Fprintf(&b, "\nTotals: %d tokens · $%.4f · %s\n",
		s.TotalUsage.TotalTokens, s.TotalCostUSD, s.Duration.Round(time.Millisecond))
	if s.BelowThreshold {
		b.WriteString("\n⚠️ Pass rate below threshold.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeFailureSections appends a collapsible <details> block per failing task.
func writeFailureSections(b *strings.Builder, rep *SuiteReport) {
	for _, task := range rep.Tasks {
		if task.Pass {
			continue
		}
		detail := failureDetail(task)
		if detail == "" {
			continue
		}
		fmt.Fprintf(b, "\n<details><summary>%s — failures</summary>\n\n", task.TaskID)
		for line := range strings.SplitSeq(detail, "\n") {
			fmt.Fprintf(b, "- %s\n", line)
		}
		b.WriteString("\n</details>\n")
	}
}

// taskAverages returns mean tokens, cost, and latency across a task's trials.
func taskAverages(task TaskReport) (int, float64, time.Duration) {
	n := len(task.Trials)
	if n == 0 {
		return 0, 0, 0
	}
	var toks int
	var cost float64
	var lat time.Duration
	for _, tr := range task.Trials {
		toks += tr.Usage.TotalTokens
		cost += tr.CostUSD
		lat += tr.Latency
	}
	return toks / n, cost / float64(n), lat / time.Duration(n)
}

// taskLabel renders a task's outcome, mirroring the JUnit writer's
// pass/skipped/failure split so the two reports agree.
func taskLabel(task TaskReport) string {
	switch {
	case task.Pass && hasOnlyUnknownGrades(task):
		return "SKIP"
	case task.Pass:
		return "PASS"
	default:
		return "FAIL"
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
