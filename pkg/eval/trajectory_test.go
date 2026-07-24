package eval

import (
	"context"
	"testing"
)

func calls(names ...string) []ToolCallRecord {
	out := make([]ToolCallRecord, len(names))
	for i, n := range names {
		out[i] = ToolCallRecord{ID: n, Name: n}
	}
	return out
}

func exp(names ...string) []ExpectedCall {
	out := make([]ExpectedCall, len(names))
	for i, n := range names {
		out[i] = ExpectedCall{Name: n}
	}
	return out
}

func TestTrajectoryModes(t *testing.T) {
	cases := []struct {
		name     string
		mode     MatchMode
		actual   []ToolCallRecord
		expected []ExpectedCall
		wantPass bool
	}{
		{"strict match", MatchStrict, calls("a", "b"), exp("a", "b"), true},
		{"strict order violation", MatchStrict, calls("b", "a"), exp("a", "b"), false},
		{"strict extra", MatchStrict, calls("a", "b", "c"), exp("a", "b"), false},
		{"strict missing", MatchStrict, calls("a"), exp("a", "b"), false},

		{"in_order subsequence", MatchInOrder, calls("a", "x", "b"), exp("a", "b"), true},
		{"in_order wrong order", MatchInOrder, calls("b", "a"), exp("a", "b"), false},
		{"in_order missing", MatchInOrder, calls("a", "x"), exp("a", "b"), false},

		{"unordered match", MatchUnordered, calls("b", "a"), exp("a", "b"), true},
		{"unordered extra fails", MatchUnordered, calls("a", "b", "c"), exp("a", "b"), false},
		{"unordered missing fails", MatchUnordered, calls("a"), exp("a", "b"), false},

		{"subset present", MatchSubset, calls("x", "a", "y", "b"), exp("a", "b"), true},
		{"subset missing", MatchSubset, calls("a", "x"), exp("a", "b"), false},
		{"subset empty expected", MatchSubset, calls("a"), exp(), true},

		{"superset within set", MatchSuperset, calls("a"), exp("a", "b"), true},
		{"superset outside set", MatchSuperset, calls("a", "z"), exp("a", "b"), false},
		{"superset empty actual", MatchSuperset, calls(), exp("a"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := Trajectory(tc.mode, tc.expected)
			grade := g.Grade(context.Background(), &Transcript{ToolCalls: tc.actual})
			if grade.Pass != tc.wantPass {
				t.Fatalf("mode=%s pass=%v want=%v reason=%q", tc.mode, grade.Pass, tc.wantPass, grade.Reason)
			}
		})
	}
}

func TestTrajectoryPartialCredit(t *testing.T) {
	g := Trajectory(MatchSubset, exp("a", "b", "c", "d"))
	grade := g.Grade(context.Background(), &Transcript{ToolCalls: calls("a", "b")})
	if grade.Pass {
		t.Fatalf("should not pass with 2 of 4")
	}
	if grade.Score != 0.5 {
		t.Fatalf("partial credit = %v, want 0.5", grade.Score)
	}
}

func TestTrajectoryArgMatchers(t *testing.T) {
	ctx := context.Background()
	tc := []ToolCallRecord{{Name: "get_weather", ArgsJSON: `{"city":"Tokyo","unit":"c"}`}}

	sub := Trajectory(MatchStrict, []ExpectedCall{{Name: "get_weather", Args: ArgsSubset(map[string]any{"city": "Tokyo"})}})
	if grade := sub.Grade(ctx, &Transcript{ToolCalls: tc}); !grade.Pass {
		t.Fatalf("ArgsSubset should match: %q", grade.Reason)
	}
	subMiss := Trajectory(MatchStrict, []ExpectedCall{{Name: "get_weather", Args: ArgsSubset(map[string]any{"city": "Osaka"})}})
	if grade := subMiss.Grade(ctx, &Transcript{ToolCalls: tc}); grade.Pass {
		t.Fatalf("ArgsSubset should not match wrong city")
	}
	exact := Trajectory(MatchStrict, []ExpectedCall{{Name: "get_weather", Args: ArgsExact(`{"unit":"c","city":"Tokyo"}`)}})
	if grade := exact.Grade(ctx, &Transcript{ToolCalls: tc}); !grade.Pass {
		t.Fatalf("ArgsExact should match regardless of key order: %q", grade.Reason)
	}
	rex := Trajectory(MatchStrict, []ExpectedCall{{Name: "get_weather", Args: ArgsRegex("city", "^Tok")}})
	if grade := rex.Grade(ctx, &Transcript{ToolCalls: tc}); !grade.Pass {
		t.Fatalf("ArgsRegex should match: %q", grade.Reason)
	}
}

func TestTrajectoryHeterogeneousMatchersOnDuplicateTool(t *testing.T) {
	// Greedy first-fit would let AnyArgs consume search{q:x}, stranding the
	// ArgsSubset{q:x} matcher; bipartite matching finds the valid assignment.
	actual := []ToolCallRecord{
		{Name: "search", ArgsJSON: `{"q":"x"}`},
		{Name: "search", ArgsJSON: `{"q":"y"}`},
	}
	expected := []ExpectedCall{
		{Name: "search", Args: AnyArgs()},
		{Name: "search", Args: ArgsSubset(map[string]any{"q": "x"})},
	}
	for _, mode := range []MatchMode{MatchUnordered, MatchSubset} {
		g := Trajectory(mode, expected)
		if grade := g.Grade(context.Background(), &Transcript{ToolCalls: actual}); !grade.Pass {
			t.Fatalf("%s: bipartite match should find valid assignment: %s", mode, grade.Reason)
		}
	}
}

func TestTrajectoryOptions(t *testing.T) {
	ctx := context.Background()
	recs := []ToolCallRecord{
		{Name: "top", ID: "1"},
		{Name: "sub", ID: "2", Source: "subagent:w"},
		{Name: "cached", ID: "3", Reused: true},
	}
	// Default: only "top" and "cached" (sub excluded), so superset over {top,cached} passes.
	g := Trajectory(MatchUnordered, exp("top", "cached"))
	if grade := g.Grade(ctx, &Transcript{ToolCalls: recs}); !grade.Pass {
		t.Fatalf("default filtering failed: %q", grade.Reason)
	}
	// IgnoreReused drops "cached", leaving only "top".
	g = Trajectory(MatchUnordered, exp("top"), TrajectoryOptions{IgnoreReused: true})
	if grade := g.Grade(ctx, &Transcript{ToolCalls: recs}); !grade.Pass {
		t.Fatalf("IgnoreReused filtering failed: %q", grade.Reason)
	}
	// IncludeSubagents brings "sub" into scope.
	g = Trajectory(MatchSubset, exp("sub"), TrajectoryOptions{IncludeSubagents: true})
	if grade := g.Grade(ctx, &Transcript{ToolCalls: recs}); !grade.Pass {
		t.Fatalf("IncludeSubagents failed: %q", grade.Reason)
	}
}
