package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// MatchMode selects how a tool-call trajectory is compared to expectations.
type MatchMode int

const (
	// MatchStrict requires the same calls in the same order with matching
	// args; any extra or missing call fails. All-or-nothing (score 0 or 1).
	MatchStrict MatchMode = iota
	// MatchInOrder requires the expected calls to appear as an ordered
	// subsequence of the actual calls; extra calls in between are allowed.
	// The relaxed default for CI gating (per Anthropic: don't mandate rigid
	// step sequences).
	MatchInOrder
	// MatchUnordered requires the same multiset of calls in any order, with
	// no extra calls.
	MatchUnordered
	// MatchSubset requires every expected call to appear somewhere, order
	// irrelevant, extra calls allowed.
	MatchSubset
	// MatchSuperset requires every actual call to match some expected entry —
	// the agent called nothing outside the expected set (but may call fewer).
	MatchSuperset
)

// String renders a MatchMode for reports and diagnostics.
func (m MatchMode) String() string {
	switch m {
	case MatchStrict:
		return "strict"
	case MatchInOrder:
		return "in_order"
	case MatchUnordered:
		return "unordered"
	case MatchSubset:
		return "subset"
	case MatchSuperset:
		return "superset"
	default:
		return "unknown"
	}
}

// ArgMatcher validates one tool call's argument JSON. It returns whether the
// args are acceptable plus a short reason used in failure diffs.
type ArgMatcher interface {
	MatchArgs(argsJSON string) (bool, string)
}

// argMatcherFunc adapts a closure to ArgMatcher.
type argMatcherFunc func(argsJSON string) (bool, string)

func (f argMatcherFunc) MatchArgs(argsJSON string) (bool, string) { return f(argsJSON) }

// AnyArgs accepts any arguments. It is also the default when ExpectedCall.Args
// is nil.
func AnyArgs() ArgMatcher {
	return argMatcherFunc(func(string) (bool, string) { return true, "" })
}

// ArgsExact matches when the call args equal wantJSON after canonicalization
// (parse both, deep-compare) so key order and whitespace do not matter. Falls
// back to raw string equality when wantJSON is not valid JSON.
func ArgsExact(wantJSON string) ArgMatcher {
	var want any
	wantOK := json.Unmarshal([]byte(wantJSON), &want) == nil
	return argMatcherFunc(func(argsJSON string) (bool, string) {
		if !wantOK {
			if argsJSON == wantJSON {
				return true, ""
			}
			return false, fmt.Sprintf("args %q != %q", argsJSON, wantJSON)
		}
		var got any
		if err := json.Unmarshal([]byte(argsJSON), &got); err != nil {
			return false, fmt.Sprintf("args not JSON: %q", argsJSON)
		}
		if reflect.DeepEqual(got, want) {
			return true, ""
		}
		return false, fmt.Sprintf("args %s != %s", argsJSON, wantJSON)
	})
}

// ArgsSubset matches when every key in want is present in the call args with
// an equal value; extra keys in the call are ignored.
func ArgsSubset(want map[string]any) ArgMatcher {
	// Canonicalize wanted values through a JSON round-trip so numeric types
	// line up with the float64 that decoding actual args produces.
	canon := make(map[string]any, len(want))
	if b, err := json.Marshal(want); err == nil {
		_ = json.Unmarshal(b, &canon)
	}
	return argMatcherFunc(func(argsJSON string) (bool, string) {
		var got map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &got); err != nil {
			return false, fmt.Sprintf("args not an object: %q", argsJSON)
		}
		for k, wv := range canon {
			gv, ok := got[k]
			if !ok {
				return false, fmt.Sprintf("args missing key %q", k)
			}
			if !reflect.DeepEqual(gv, wv) {
				return false, fmt.Sprintf("args[%q] = %v, want %v", k, gv, wv)
			}
		}
		return true, ""
	})
}

// ArgsRegex matches when the string value at top-level key matches pattern.
// It compiles once and panics on a bad pattern (like regexp.MustCompile).
func ArgsRegex(key, pattern string) ArgMatcher {
	re := regexp.MustCompile(pattern)
	return argMatcherFunc(func(argsJSON string) (bool, string) {
		var got map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &got); err != nil {
			return false, fmt.Sprintf("args not an object: %q", argsJSON)
		}
		s, ok := got[key].(string)
		if !ok {
			return false, fmt.Sprintf("args[%q] is not a string", key)
		}
		if re.MatchString(s) {
			return true, ""
		}
		return false, fmt.Sprintf("args[%q]=%q does not match /%s/", key, s, pattern)
	})
}

// ExpectedCall is one entry in an expected trajectory. Args nil means AnyArgs.
type ExpectedCall struct {
	Name string
	Args ArgMatcher
}

func (e ExpectedCall) matches(a ToolCallRecord) bool {
	if a.Name != e.Name {
		return false
	}
	m := e.Args
	if m == nil {
		m = AnyArgs()
	}
	ok, _ := m.MatchArgs(a.ArgsJSON)
	return ok
}

// TrajectoryOptions tunes which recorded calls participate in matching.
type TrajectoryOptions struct {
	IncludeSubagents bool // match against Source != "" calls too
	IgnoreReused     bool // skip cache-hit (Reused) calls
}

// Trajectory returns a code-based grader over Transcript.ToolCalls using the
// given match mode. Score carries partial credit for subsequence/subset modes;
// Reason carries a positional or set diff on failure.
func Trajectory(mode MatchMode, calls []ExpectedCall, opts ...TrajectoryOptions) Grader {
	name := "trajectory:" + mode.String()
	var o TrajectoryOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	return GraderFunc(name, func(_ context.Context, tr *Transcript) Grade {
		actual := filterCalls(tr.ToolCalls, o)
		pass, score, reason := matchTrajectory(mode, actual, calls)
		return Grade{Grader: name, Pass: pass, Score: score, Reason: reason}
	})
}

// filterCalls applies the subagent/reused options to the recorded calls.
func filterCalls(calls []ToolCallRecord, o TrajectoryOptions) []ToolCallRecord {
	out := make([]ToolCallRecord, 0, len(calls))
	for _, c := range calls {
		if c.Source != "" && !o.IncludeSubagents {
			continue
		}
		if c.Reused && o.IgnoreReused {
			continue
		}
		out = append(out, c)
	}
	return out
}

// matchTrajectory dispatches to the per-mode matcher and returns
// (pass, score, reason).
func matchTrajectory(mode MatchMode, actual []ToolCallRecord, expected []ExpectedCall) (bool, float64, string) {
	switch mode {
	case MatchStrict:
		return matchStrict(actual, expected)
	case MatchInOrder:
		return matchInOrder(actual, expected)
	case MatchUnordered:
		return matchUnordered(actual, expected)
	case MatchSubset:
		return matchSubset(actual, expected)
	case MatchSuperset:
		return matchSuperset(actual, expected)
	default:
		return false, 0, "unknown match mode"
	}
}

func matchStrict(actual []ToolCallRecord, expected []ExpectedCall) (bool, float64, string) {
	if len(actual) != len(expected) {
		return false, 0, fmt.Sprintf("got %d calls, want %d", len(actual), len(expected))
	}
	for i, e := range expected {
		if !e.matches(actual[i]) {
			return false, 0, fmt.Sprintf("step %d: got %s, want %s", i, actual[i].Name, e.Name)
		}
	}
	return true, 1, ""
}

func matchInOrder(actual []ToolCallRecord, expected []ExpectedCall) (bool, float64, string) {
	j := 0
	for _, a := range actual {
		if j < len(expected) && expected[j].matches(a) {
			j++
		}
	}
	return credit(j, len(expected), func() string {
		return fmt.Sprintf("matched %d of %d expected in order; first unmatched: %s", j, len(expected), expected[j].Name)
	})
}

func matchUnordered(actual []ToolCallRecord, expected []ExpectedCall) (bool, float64, string) {
	if len(actual) != len(expected) {
		return false, 0, fmt.Sprintf("got %d calls, want %d (unordered requires equal counts)", len(actual), len(expected))
	}
	return matchSetGreedy(actual, expected)
}

func matchSubset(actual []ToolCallRecord, expected []ExpectedCall) (bool, float64, string) {
	return matchSetGreedy(actual, expected)
}

// matchSetGreedy consumes one actual call per expected call (order-free).
func matchSetGreedy(actual []ToolCallRecord, expected []ExpectedCall) (bool, float64, string) {
	consumed := make([]bool, len(actual))
	matched := 0
	var missing string
	for _, e := range expected {
		found := false
		for i, a := range actual {
			if !consumed[i] && e.matches(a) {
				consumed[i] = true
				matched++
				found = true
				break
			}
		}
		if !found && missing == "" {
			missing = e.Name
		}
	}
	return credit(matched, len(expected), func() string {
		return fmt.Sprintf("matched %d of %d expected; missing: %s", matched, len(expected), missing)
	})
}

func matchSuperset(actual []ToolCallRecord, expected []ExpectedCall) (bool, float64, string) {
	for i, a := range actual {
		ok := false
		for _, e := range expected {
			if e.matches(a) {
				ok = true
				break
			}
		}
		if !ok {
			return false, 0, fmt.Sprintf("step %d: unexpected call %s (outside expected set)", i, a.Name)
		}
	}
	return true, 1, ""
}

// credit builds a (pass, score, reason) result from matched/total counts.
// total==0 is a vacuous pass. reason is evaluated lazily only on failure.
func credit(matched, total int, reason func() string) (bool, float64, string) {
	if total == 0 {
		return true, 1, ""
	}
	if matched == total {
		return true, 1, ""
	}
	return false, float64(matched) / float64(total), strings.TrimSpace(reason())
}
