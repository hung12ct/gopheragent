package agent

import (
	"context"
	"testing"
)

func TestPayload_LimitExhaustedCarriesAllFields(t *testing.T) {
	ev := Event(LimitExhaustedEvent{
		Kind:  LimitKindMaxToolCallsPerSession,
		Limit: 5,
		Used:  7,
	})
	p, ok := ev.Payload.(LimitExhaustedEvent)
	if !ok {
		t.Fatalf("expected LimitExhaustedEvent, got %T", ev.Payload)
	}
	if p.Kind != LimitKindMaxToolCallsPerSession {
		t.Fatalf("Kind: got %q, want %q", p.Kind, LimitKindMaxToolCallsPerSession)
	}
	if p.Limit != 5 || p.Used != 7 {
		t.Fatalf("limit/used: got %d/%d, want 5/7", p.Limit, p.Used)
	}
}

func TestLimitExhaustedStreamEvent_RoundTripsThroughPayload(t *testing.T) {
	ev := LimitExhaustedStreamEvent(LimitKindProviderMaxTokens, 4096, 0)
	p := ev.Payload.(LimitExhaustedEvent)
	if p.Kind != LimitKindProviderMaxTokens || p.Limit != 4096 {
		t.Fatalf("round-trip: got %+v", p)
	}
}

func TestLimitExhaustedEvent_ReasonCarriesIncompleteToolUse(t *testing.T) {
	// The Anthropic provider emits this exact shape when Accumulate fails
	// mid-tool_use; adopters that auto-retry on truncation read .Reason to
	// decide whether the response is salvageable or strictly needs a retry.
	ev := Event(LimitExhaustedEvent{
		Kind:   LimitKindProviderMaxTokens,
		Limit:  8192,
		Reason: LimitReasonIncompleteToolUse,
	})
	p, ok := ev.Payload.(LimitExhaustedEvent)
	if !ok {
		t.Fatalf("expected LimitExhaustedEvent, got %T", ev.Payload)
	}
	if p.Reason != LimitReasonIncompleteToolUse {
		t.Fatalf("Reason: got %q, want %q", p.Reason, LimitReasonIncompleteToolUse)
	}
	// Sanity-check the zero-Reason case still round-trips for existing kinds.
	plain := Event(LimitExhaustedEvent{Kind: LimitKindMaxIters, Limit: 15}).Payload.(LimitExhaustedEvent)
	if plain.Reason != "" {
		t.Fatalf("zero Reason must remain empty for non-tool-use kinds, got %q", plain.Reason)
	}
}

func TestMaxIters_AlsoEmitsLimitExhausted(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(1)},
		{ToolCalls: fanoutCalls(1)},
		{ToolCalls: fanoutCalls(1)},
		{ToolCalls: fanoutCalls(1)},
	}}
	loop, _ := setup(provider, ct)
	loop.MaxIters = 2

	var sawLimitExhausted, sawMaxItersReached bool
	for ev := range loop.RunText(context.Background(), "s1", "go") {
		switch p := ev.Payload.(type) {
		case LimitExhaustedEvent:
			if p.Kind == LimitKindMaxIters {
				sawLimitExhausted = true
			}
		case MaxItersReachedEvent:
			sawMaxItersReached = true
		}
	}
	if !sawLimitExhausted {
		t.Fatal("expected LimitExhaustedEvent with kind=max_iters when MaxIters cap fires")
	}
	if !sawMaxItersReached {
		t.Fatal("legacy MaxItersReachedEvent must still fire alongside the new event for back-compat")
	}
}

func TestMaxToolCallsPerSession_AlsoEmitsLimitExhausted(t *testing.T) {
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(2)},
		{ToolCalls: fanoutCalls(2)},
	}}
	loop, _ := setup(provider, ct)
	loop.MaxToolCallsPerSession = 3

	var seen LimitExhaustedEvent
	var found bool
	for ev := range loop.RunText(context.Background(), "s1", "go") {
		if p, ok := ev.Payload.(LimitExhaustedEvent); ok && p.Kind == LimitKindMaxToolCallsPerSession {
			seen = p
			found = true
		}
	}
	if !found {
		t.Fatal("expected LimitExhaustedEvent with kind=max_tool_calls_per_session")
	}
	if seen.Limit != 3 {
		t.Fatalf("Limit: got %d, want 3", seen.Limit)
	}
	if seen.Used < seen.Limit {
		t.Fatalf("Used should be >= Limit at trip time, got used=%d limit=%d", seen.Used, seen.Limit)
	}
}
