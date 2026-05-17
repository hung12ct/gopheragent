package agent

import (
	"context"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

func TestIsFreshSessionHistory(t *testing.T) {
	cases := []struct {
		name string
		msgs []history.Message
		want bool
	}{
		{"empty", nil, true},
		{"system-only", []history.Message{{Role: "system", Content: "you are…"}}, true},
		{"system+user", []history.Message{{Role: "system"}, {Role: "user", Content: "hi"}}, false},
		{"single-user", []history.Message{{Role: "user", Content: "hi"}}, false},
	}
	for _, tc := range cases {
		if got := isFreshSessionHistory(tc.msgs); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSessionCreated_EmittedOnFirstTurnOnly(t *testing.T) {
	provider := &scriptProvider{turns: []LLMResult{
		{Content: "hi"},
		{Content: "hi again"},
	}}
	loop, _ := setup(provider)

	drain := func(userInput string) []StreamEvent {
		var out []StreamEvent
		for ev := range loop.RunText(context.Background(), "fresh:s1", userInput) {
			out = append(out, ev)
		}
		return out
	}
	firstTurnEvents := drain("first user msg")
	secondTurnEvents := drain("second user msg")

	countSessionCreated := func(evs []StreamEvent) int {
		n := 0
		for _, ev := range evs {
			if ev.Type == EventTypeSessionCreated {
				n++
			}
		}
		return n
	}

	if got := countSessionCreated(firstTurnEvents); got != 1 {
		t.Fatalf("first turn should emit exactly one session_created, got %d", got)
	}
	if got := countSessionCreated(secondTurnEvents); got != 0 {
		t.Fatalf("second turn must not re-emit session_created, got %d", got)
	}
}
