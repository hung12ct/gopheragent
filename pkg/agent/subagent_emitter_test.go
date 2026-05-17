package agent

import (
	"context"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// injectingProvider emits a normal content chunk plus a simulated "sub-agent
// forwarded" event on the same channel, so tests can assert RunIteration's
// filtering behavior end-to-end.
type injectingProvider struct{}

func (injectingProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	ch <- Event(ContentEvent{Text: "parent-answer"})
	sub := Event(ContentEvent{Text: "subagent-chatter"})
	sub.Source = "subagent:child"
	sub.ParentID = "p"
	ch <- sub
	return LLMResult{Content: "parent-answer"}, nil
}

func TestWithSubAgentEmitter_RoundTrip(t *testing.T) {
	var got StreamEvent
	ctx := WithSubAgentEmitter(context.Background(), func(ev StreamEvent) { got = ev })
	fn := SubAgentEmitterFromContext(ctx)
	if fn == nil {
		t.Fatal("emitter not retrievable from ctx")
	}
	fn(Event(ContentEvent{Text: "hello"}))
	if p, ok := got.Payload.(ContentEvent); !ok || p.Text != "hello" {
		t.Fatalf("emitter not invoked or payload wrong: got %+v", got)
	}
}

func TestSubAgentEmitterFromContext_NilWhenAbsent(t *testing.T) {
	if SubAgentEmitterFromContext(context.Background()) != nil {
		t.Fatal("expected nil emitter outside a loop")
	}
}

func TestDecorateForwardedEvent_SetsSourceAndParentID(t *testing.T) {
	ev := DecorateForwardedEvent(Event(ContentEvent{Text: "x"}), "subagent:A", "sess-1")
	if ev.Source != "subagent:A" {
		t.Fatalf("Source: got %q", ev.Source)
	}
	if ev.ParentID != "sess-1" {
		t.Fatalf("ParentID: got %q", ev.ParentID)
	}
}

func TestDecorateForwardedEvent_ChainsNestedSources(t *testing.T) {
	// Inner sub-agent B tagged its event first.
	ev := DecorateForwardedEvent(StreamEvent{Type: "thought"}, "subagent:B", "sess-inner")
	// Outer sub-agent A now forwards that event upward.
	ev = DecorateForwardedEvent(ev, "subagent:A", "sess-outer")

	if ev.Source != "subagent:A>subagent:B" {
		t.Fatalf("expected chained Source 'subagent:A>subagent:B', got %q", ev.Source)
	}
	// ParentID always reflects the *outer* forwarder — by the time it reaches
	// the user, ParentID points at the user-facing session.
	if ev.ParentID != "sess-outer" {
		t.Fatalf("expected ParentID to be overwritten with outer session, got %q", ev.ParentID)
	}
}

func TestRunIteration_SkipsSubAgentEvents(t *testing.T) {
	// A fake provider that emits a normal content chunk plus simulates a
	// sub-agent's content being forwarded into the same channel. RunIteration
	// must return only the parent's content in its final string.
	loop, _ := setup(injectingProvider{})
	got, err := loop.RunIteration(context.Background(), "p", "go")
	if err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if got != "parent-answer" {
		t.Fatalf("final content must exclude forwarded sub-agent events; got %q", got)
	}
}
