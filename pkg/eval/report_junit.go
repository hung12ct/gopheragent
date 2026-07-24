package eval

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// junitTestsuites is the root of a JUnit XML document. GitHub Actions,
// GitLab, and Jenkins render it natively in their test panes.
type junitTestsuites struct {
	XMLName  xml.Name         `xml:"testsuites"`
	Name     string           `xml:"name,attr"`
	Tests    int              `xml:"tests,attr"`
	Failures int              `xml:"failures,attr"`
	Skipped  int              `xml:"skipped,attr"`
	Time     float64          `xml:"time,attr"`
	Suites   []junitTestsuite `xml:"testsuite"`
}

type junitTestsuite struct {
	Name     string          `xml:"name,attr"`
	Tests    int             `xml:"tests,attr"`
	Failures int             `xml:"failures,attr"`
	Skipped  int             `xml:"skipped,attr"`
	Time     float64         `xml:"time,attr"`
	Cases    []junitTestcase `xml:"testcase"`
}

type junitTestcase struct {
	Name    string        `xml:"name,attr"`
	Time    float64       `xml:"time,attr"`
	Failure *junitFailure `xml:"failure,omitempty"`
	Skipped *junitSkipped `xml:"skipped,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr,omitempty"`
}

// WriteJUnit writes the report as JUnit XML: one testcase per task, a
// <failure> carrying the failing grade reasons, and <skipped> for tasks whose
// graders were all inconclusive (judge returned unknown).
func WriteJUnit(w io.Writer, rep *SuiteReport) error {
	suite := junitTestsuite{Name: rep.Suite, Tests: len(rep.Tasks), Time: rep.Summary.Duration.Seconds()}
	for _, task := range rep.Tasks {
		tc := junitTestcase{Name: task.TaskID, Time: taskSeconds(task)}
		switch {
		case task.Pass && hasOnlyUnknownGrades(task):
			tc.Skipped = &junitSkipped{Message: "judge inconclusive"}
			suite.Skipped++
		case !task.Pass:
			detail := failureDetail(task)
			tc.Failure = &junitFailure{Message: failureSummary(task), Body: detail}
			suite.Failures++
		}
		suite.Cases = append(suite.Cases, tc)
	}
	doc := junitTestsuites{
		Name: rep.Suite, Tests: suite.Tests, Failures: suite.Failures,
		Skipped: suite.Skipped, Time: suite.Time, Suites: []junitTestsuite{suite},
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return fmt.Errorf("eval: write junit: %w", err)
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("eval: write junit: %w", err)
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// taskSeconds sums a task's trial latencies in seconds.
func taskSeconds(task TaskReport) float64 {
	var total float64
	for _, tr := range task.Trials {
		total += tr.Latency.Seconds()
	}
	return total
}

// failureSummary is the one-line message on the <failure> element.
func failureSummary(task TaskReport) string {
	passed := 0
	for _, tr := range task.Trials {
		if tr.Pass {
			passed++
		}
	}
	return fmt.Sprintf("%d/%d trials passed", passed, len(task.Trials))
}

// failureDetail concatenates the failing grade reasons and harness errors
// across a task's failing trials.
func failureDetail(task TaskReport) string {
	var b strings.Builder
	for _, tr := range task.Trials {
		if tr.Pass {
			continue
		}
		if tr.Error != "" {
			fmt.Fprintf(&b, "trial %d: %s\n", tr.Trial, tr.Error)
		}
		for _, turn := range tr.Turns {
			for _, g := range turn.Grades {
				switch {
				case g.Err != nil:
					fmt.Fprintf(&b, "trial %d turn %d: %s errored: %v\n", tr.Trial, turn.TurnIndex, g.Grader, g.Err)
				case !g.Pass && !g.Unknown:
					fmt.Fprintf(&b, "trial %d turn %d: %s: %s\n", tr.Trial, turn.TurnIndex, g.Grader, g.Reason)
				}
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// hasOnlyUnknownGrades reports whether a task had graders and every one was
// Unknown — the "judge couldn't decide" case worth surfacing as skipped.
func hasOnlyUnknownGrades(task TaskReport) bool {
	sawGrade, sawDecisive := false, false
	for _, tr := range task.Trials {
		for _, turn := range tr.Turns {
			for _, g := range turn.Grades {
				sawGrade = true
				if !g.Unknown {
					sawDecisive = true
				}
			}
		}
	}
	return sawGrade && !sawDecisive
}
