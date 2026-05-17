package agent

import (
	"context"
	"sync"
	"testing"
)

// drainerHarness builds the minimal state a streamDrainer needs.
func drainerHarness(t *testing.T) (*streamDrainer, *iterationState, chan StreamEvent) {
	t.Helper()
	out := make(chan StreamEvent, 16)
	st := &iterationState{
		sessionKey: "s1",
		streamChan: out,
		specMap:    newSpeculativeMap(),
		specMu:     &sync.Mutex{},
	}
	d := newStreamDrainer(&AgentLoop{}, st, context.Background())
	return d, st, out
}

func TestStreamDrainer_AccumulatesContent(t *testing.T) {
	d, _, _ := drainerHarness(t)
	if !d.handleEvent(Event(ContentEvent{Text: "Hello "})) {
		t.Fatal("expected handleEvent to forward")
	}
	if !d.handleEvent(Event(ContentEvent{Text: "world"})) {
		t.Fatal("expected handleEvent to forward")
	}
	if got := d.content(); got != "Hello world" {
		t.Fatalf("content = %q; want %q", got, "Hello world")
	}
	if !d.contentEmitted() {
		t.Fatal("contentEmitted should be true after content events")
	}
}

func TestStreamDrainer_ForwardsAllEvents(t *testing.T) {
	d, _, out := drainerHarness(t)
	events := []StreamEvent{
		Event(ThoughtEvent{Message: "thinking"}),
		Event(ContentEvent{Text: "hi"}),
		Event(ToolCallEvent{Name: "x"}),
	}
	for _, ev := range events {
		if !d.handleEvent(ev) {
			t.Fatalf("forward dropped event: %+v", ev)
		}
	}
	for i, want := range events {
		got := <-out
		if got.Type != want.Type {
			t.Fatalf("event %d: got type %q; want %q", i, got.Type, want.Type)
		}
	}
}

func TestStreamDrainer_StopsForwardingOnCtxDone(t *testing.T) {
	out := make(chan StreamEvent) // unbuffered → forces select to ctx.Done branch
	st := &iterationState{
		sessionKey: "s1",
		streamChan: out,
		specMap:    newSpeculativeMap(),
		specMu:     &sync.Mutex{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := newStreamDrainer(&AgentLoop{}, st, ctx)

	if d.handleEvent(Event(ContentEvent{Text: "x"})) {
		t.Fatal("handleEvent should return false when ctx is cancelled and streamChan blocks")
	}
}
