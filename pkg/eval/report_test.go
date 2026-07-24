package eval

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

// sampleReport builds a report with one passing, one failing, and one
// judge-inconclusive (skipped) task.
func sampleReport() *SuiteReport {
	passing := TaskReport{TaskID: "ok", Pass: true, PassAtK: true, PassAllK: true, Trials: []TrialReport{{
		TaskID: "ok", Trial: 1, Pass: true, Latency: 2 * time.Second,
		Turns: []TurnReport{{Grades: []Grade{{Grader: "contains", Pass: true, Score: 1}}, Pass: true}},
	}}}
	failing := TaskReport{TaskID: "bad", Pass: false, Trials: []TrialReport{{
		TaskID: "bad", Trial: 1, Pass: false, Latency: time.Second,
		Turns: []TurnReport{{Grades: []Grade{{Grader: "contains", Pass: false, Reason: "missing \"Paris\""}}}},
	}}}
	skipped := TaskReport{TaskID: "maybe", Pass: true, PassAtK: true, PassAllK: true, Trials: []TrialReport{{
		TaskID: "maybe", Trial: 1, Pass: true,
		Turns: []TurnReport{{Grades: []Grade{{Grader: "judge", Unknown: true}}, Pass: true}},
	}}}
	tasks := []TaskReport{passing, failing, skipped}
	return &SuiteReport{
		Suite: "demo", StartedAt: time.Unix(0, 0), Tasks: tasks,
		Summary: summarize(tasks, 0.9, 3*time.Second),
	}
}

func TestWriteJSONRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, sampleReport()); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var back SuiteReport
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if back.Suite != "demo" || len(back.Tasks) != 3 {
		t.Fatalf("round-trip mismatch: %+v", back.Summary)
	}
	if back.Summary.Passed != 2 || back.Summary.Failed != 1 {
		t.Fatalf("summary counts = %+v", back.Summary)
	}
}

func TestWriteJUnit(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJUnit(&buf, sampleReport()); err != nil {
		t.Fatalf("WriteJUnit: %v", err)
	}
	var doc struct {
		Tests    int `xml:"tests,attr"`
		Failures int `xml:"failures,attr"`
		Skipped  int `xml:"skipped,attr"`
		Suites   []struct {
			Cases []struct {
				Name    string `xml:"name,attr"`
				Failure *struct {
					Message string `xml:"message,attr"`
					Body    string `xml:",chardata"`
				} `xml:"failure"`
				Skipped *struct{} `xml:"skipped"`
			} `xml:"testcase"`
		} `xml:"testsuite"`
	}
	if err := xml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("xml unmarshal: %v\n%s", err, buf.String())
	}
	if doc.Tests != 3 || doc.Failures != 1 || doc.Skipped != 1 {
		t.Fatalf("junit counts: tests=%d failures=%d skipped=%d", doc.Tests, doc.Failures, doc.Skipped)
	}
	cases := doc.Suites[0].Cases
	if len(cases) != 3 {
		t.Fatalf("expected 3 testcases, got %d", len(cases))
	}
	var badFailure string
	for _, c := range cases {
		if c.Name == "bad" && c.Failure != nil {
			badFailure = c.Failure.Body
		}
	}
	if !strings.Contains(badFailure, "Paris") {
		t.Fatalf("failure body should carry grade reason, got %q", badFailure)
	}
}

func TestWriteJUnitSanitizesControlChars(t *testing.T) {
	rep := &SuiteReport{Suite: "s", Tasks: []TaskReport{{TaskID: "bad", Pass: false, Trials: []TrialReport{{
		Trial: 1, Pass: false,
		Turns: []TurnReport{{Grades: []Grade{{Grader: "contains", Reason: "got \x00\x01 junk"}}}},
	}}}}}
	var buf bytes.Buffer
	if err := WriteJUnit(&buf, rep); err != nil {
		t.Fatalf("WriteJUnit: %v", err)
	}
	if bytes.ContainsAny(buf.Bytes(), "\x00\x01") {
		t.Fatalf("control chars leaked into JUnit XML")
	}
	var doc struct{}
	if err := xml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("sanitized XML must still parse: %v", err)
	}
}

func TestWriteMarkdown(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, sampleReport()); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Eval: demo", "2/3 passed", "| ok | PASS |", "| bad | FAIL |", "| maybe | SKIP |", "Paris", "below threshold"} {
		if !strings.Contains(out, want) {
			t.Fatalf("markdown missing %q\n%s", want, out)
		}
	}
}
