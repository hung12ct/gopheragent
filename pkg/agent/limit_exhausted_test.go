package agent

import (
	"context"
	"testing"
)

func TestPayload_LimitExhaustedDecodesAllFields(t *testing.T) {
	ev := StreamEvent{
		Type:    EventTypeLimitExhausted,
		Content: `{"kind":"max_tool_calls_per_session","limit":5,"used":7}`,
	}
	p, ok := ev.Payload().(LimitExhaustedEvent)
	if !ok {
		t.Fatalf("expected LimitExhaustedEvent, got %T", ev.Payload())
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
	p := ev.Payload().(LimitExhaustedEvent)
	if p.Kind != LimitKindProviderMaxTokens || p.Limit != 4096 {
		t.Fatalf("round-trip: got %+v", p)
	}
}

func TestMaxIters_AlsoEmitsLimitExhausted(t *testing.T) {
	// runIteration exhausts MaxIters because the script never returns a
	// final answer (always wants more tool calls but we have no tools, so
	// it treats Content as final on first turn). To force the cap, set
	// MaxIters=1 and have the LLM keep emitting tool calls — but no tools
	// registered means we don't even enter the loop. The cleanest forcing
	// method: zero turns scripted → scriptProvider returns "done" once and
	// the loop ends naturally. To exhaust MaxIters we need the model to
	// keep wanting tools. Use fanoutCalls + countingTool from tool_budget_test.go
	// to keep iteration alive past MaxIters.
	ct := &countingTool{name: "counter"}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: fanoutCalls(1)},
		{ToolCalls: fanoutCalls(1)},
		{ToolCalls: fanoutCalls(1)},
		{ToolCalls: fanoutCalls(1)},
	}}
	loop, _ := setup(provider, ct)
	loop.MaxIters = 2

	ch := make(chan StreamEvent, 64)
	go loop.RunIterationStream(context.Background(), "s1", "go", ch)

	var sawLimitExhausted, sawMaxItersReached bool
	for ev := range ch {
		if ev.Type == EventTypeLimitExhausted {
			p := ev.Payload().(LimitExhaustedEvent)
			if p.Kind == LimitKindMaxIters {
				sawLimitExhausted = true
			}
		}
		if ev.Type == EventTypeMaxItersReached {
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

	ch := make(chan StreamEvent, 64)
	go loop.RunIterationStream(context.Background(), "s1", "go", ch)

	var seen LimitExhaustedEvent
	var found bool
	for ev := range ch {
		if ev.Type == EventTypeLimitExhausted {
			p := ev.Payload().(LimitExhaustedEvent)
			if p.Kind == LimitKindMaxToolCallsPerSession {
				seen = p
				found = true
			}
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
