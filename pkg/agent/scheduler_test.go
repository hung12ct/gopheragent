package agent

import (
	"strings"
	"testing"
)

func TestParseRefs(t *testing.T) {
	cases := []struct {
		name string
		args string
		want []Ref
	}{
		{"none", `{"x":1}`, nil},
		{"bare", `{"uid": <output_of:t1>}`, []Ref{{ID: "t1"}}},
		{"with-path", `{"uid": <output_of:t1.user_id>}`, []Ref{{ID: "t1", Path: "user_id"}}},
		{"nested-path", `{"uid": <output_of:t1.user.id>}`, []Ref{{ID: "t1", Path: "user.id"}}},
		{"quoted", `{"uid": "<output_of:t1.user_id>"}`, []Ref{{ID: "t1", Path: "user_id"}}},
		{"multi", `{"a": <output_of:t1>, "b": <output_of:t2.field>}`, []Ref{
			{ID: "t1"}, {ID: "t2", Path: "field"},
		}},
		{"duplicate-ids", `{"a": <output_of:t1>, "b": <output_of:t1>}`, []Ref{
			{ID: "t1"}, {ID: "t1"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseRefs(tc.args)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d refs, want %d: %v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ref[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSubstitute_Simple(t *testing.T) {
	resolve := func(id string) (string, bool) {
		if id == "t1" {
			return `{"user_id": "abc123", "name": "Jane", "count": 42}`, true
		}
		return "", false
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"full-object", `{"filter": <output_of:t1>}`,
			`{"filter": {"count":42,"name":"Jane","user_id":"abc123"}}`},
		{"string-field", `{"user_id": <output_of:t1.user_id>}`,
			`{"user_id": "abc123"}`},
		{"number-field", `{"count": <output_of:t1.count>}`,
			`{"count": 42}`},
		{"quoted-token-becomes-scalar", `{"c": "<output_of:t1.count>"}`,
			`{"c": 42}`},
		{"missing-path-becomes-null", `{"x": <output_of:t1.nonexistent>}`,
			`{"x": null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Substitute(tc.in, resolve)
			if err != nil {
				t.Fatalf("Substitute: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSubstitute_NestedPath(t *testing.T) {
	resolve := func(id string) (string, bool) {
		if id == "t1" {
			return `{"user": {"id": "xyz", "profile": {"age": 30}}}`, true
		}
		return "", false
	}
	got, err := Substitute(`{"age": <output_of:t1.user.profile.age>}`, resolve)
	if err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	if got != `{"age": 30}` {
		t.Fatalf("got %q", got)
	}
}

func TestSubstitute_PlainStringOutput(t *testing.T) {
	resolve := func(id string) (string, bool) {
		if id == "t1" {
			return "not-valid-json", true
		}
		return "", false
	}
	// Full output: plain string is treated as a JSON string.
	got, err := Substitute(`{"r": <output_of:t1>}`, resolve)
	if err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	if got != `{"r": "not-valid-json"}` {
		t.Fatalf("got %q", got)
	}
	// Path on non-JSON output: resolves to null.
	got, err = Substitute(`{"r": <output_of:t1.field>}`, resolve)
	if err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	if got != `{"r": null}` {
		t.Fatalf("got %q", got)
	}
}

func TestSubstitute_UnknownID(t *testing.T) {
	resolve := func(_ string) (string, bool) { return "", false }
	_, err := Substitute(`{"x": <output_of:nope>}`, resolve)
	if err == nil || !strings.Contains(err.Error(), "unknown tool-call reference") {
		t.Fatalf("expected unknown-reference error, got %v", err)
	}
}

func TestSchedule_NoDeps(t *testing.T) {
	calls := []PendingToolCall{
		{ID: "a", Name: "one", ArgsJSON: `{}`},
		{ID: "b", Name: "two", ArgsJSON: `{}`},
	}
	waves, err := ScheduleToolCalls(calls)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if len(waves) != 1 || len(waves[0]) != 2 {
		t.Fatalf("expected 1 wave of 2 calls, got %v", waves)
	}
}

func TestSchedule_LinearChain(t *testing.T) {
	calls := []PendingToolCall{
		{ID: "t1", Name: "a", ArgsJSON: `{}`},
		{ID: "t2", Name: "b", ArgsJSON: `{"x": <output_of:t1.f>}`},
		{ID: "t3", Name: "c", ArgsJSON: `{"y": <output_of:t2.f>}`},
	}
	waves, err := ScheduleToolCalls(calls)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if len(waves) != 3 {
		t.Fatalf("expected 3 waves, got %d: %v", len(waves), waves)
	}
	expected := [][]string{{"t1"}, {"t2"}, {"t3"}}
	for i, w := range waves {
		if len(w) != len(expected[i]) || w[0].ID != expected[i][0] {
			t.Fatalf("wave %d = %v, want %v", i, w, expected[i])
		}
	}
}

func TestSchedule_Diamond(t *testing.T) {
	// a → b, a → c, (b,c) → d
	calls := []PendingToolCall{
		{ID: "a", ArgsJSON: `{}`},
		{ID: "b", ArgsJSON: `{"x": <output_of:a>}`},
		{ID: "c", ArgsJSON: `{"y": <output_of:a>}`},
		{ID: "d", ArgsJSON: `{"p": <output_of:b>, "q": <output_of:c>}`},
	}
	waves, err := ScheduleToolCalls(calls)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if len(waves) != 3 {
		t.Fatalf("expected 3 waves, got %d: %v", len(waves), waves)
	}
	if waves[0][0].ID != "a" {
		t.Fatalf("wave 0 expected [a], got %v", waves[0])
	}
	if len(waves[1]) != 2 {
		t.Fatalf("wave 1 expected [b,c], got %v", waves[1])
	}
	if waves[2][0].ID != "d" {
		t.Fatalf("wave 2 expected [d], got %v", waves[2])
	}
}

func TestSchedule_SelfReference(t *testing.T) {
	calls := []PendingToolCall{
		{ID: "t1", ArgsJSON: `{"x": <output_of:t1>}`},
	}
	_, err := ScheduleToolCalls(calls)
	if err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("expected self-reference error, got %v", err)
	}
}

func TestSchedule_Cycle(t *testing.T) {
	calls := []PendingToolCall{
		{ID: "t1", ArgsJSON: `{"x": <output_of:t2>}`},
		{ID: "t2", ArgsJSON: `{"y": <output_of:t1>}`},
	}
	_, err := ScheduleToolCalls(calls)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestSchedule_UnknownRef(t *testing.T) {
	calls := []PendingToolCall{
		{ID: "t1", ArgsJSON: `{"x": <output_of:nonexistent>}`},
	}
	_, err := ScheduleToolCalls(calls)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected unknown-ref error, got %v", err)
	}
}

func TestSchedule_DuplicateID(t *testing.T) {
	calls := []PendingToolCall{
		{ID: "same"},
		{ID: "same"},
	}
	_, err := ScheduleToolCalls(calls)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestSchedule_Empty(t *testing.T) {
	waves, err := ScheduleToolCalls(nil)
	if err != nil || waves != nil {
		t.Fatalf("expected (nil,nil) for empty input, got %v,%v", waves, err)
	}
}

func TestSchedule_PreservesInputOrderWithinWave(t *testing.T) {
	// Three independent calls: should all be wave 0 in input order.
	calls := []PendingToolCall{
		{ID: "z"},
		{ID: "a"},
		{ID: "m"},
	}
	waves, err := ScheduleToolCalls(calls)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if len(waves) != 1 || len(waves[0]) != 3 {
		t.Fatalf("expected 1 wave of 3, got %v", waves)
	}
	wantIDs := []string{"z", "a", "m"}
	for i, c := range waves[0] {
		if c.ID != wantIDs[i] {
			t.Fatalf("wave[0][%d] = %q, want %q", i, c.ID, wantIDs[i])
		}
	}
}
