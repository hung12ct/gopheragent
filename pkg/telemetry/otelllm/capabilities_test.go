package otelllm_test

import (
	"context"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/telemetry/otelllm"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// capableFake declares capabilities in addition to implementing LLMProvider.
type capableFake struct{ caps agent.LLMCapabilities }

func (f *capableFake) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- agent.StreamEvent) (agent.LLMResult, error) {
	return agent.LLMResult{}, nil
}

func (f *capableFake) Capabilities() agent.LLMCapabilities { return f.caps }

// Wrapping for tracing must not erase a capability the caller checks for —
// otherwise enabling telemetry silently disables a consumer's fail-fast guard.
func TestNewProviderForwardsCapabilities(t *testing.T) {
	_, _, tp, mp := newRecorders(t)
	want := agent.LLMCapabilities{ImageInput: true, StructuredOutput: true}

	wrapped := otelllm.NewProvider(&capableFake{caps: want},
		otelllm.WithTracer(tp.Tracer("test")), otelllm.WithMeter(mp.Meter("test")))

	c, ok := wrapped.(agent.CapabilityProvider)
	if !ok {
		t.Fatalf("wrapped provider does not implement agent.CapabilityProvider")
	}
	if got := c.Capabilities(); got != want {
		t.Fatalf("Capabilities() = %+v, want %+v", got, want)
	}
}

// The mirror case: a provider that makes no claim must stay unknown. Wrapping
// it must not manufacture a zero-value report that reads as "supports nothing".
func TestNewProviderDoesNotInventCapabilities(t *testing.T) {
	_, _, tp, mp := newRecorders(t)

	wrapped := otelllm.NewProvider(&fakeProvider{},
		otelllm.WithTracer(tp.Tracer("test")), otelllm.WithMeter(mp.Meter("test")))

	if _, ok := wrapped.(agent.CapabilityProvider); ok {
		t.Fatalf("wrapping an undeclared provider must not implement agent.CapabilityProvider")
	}
}
