package llm

import (
	"context"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// capableStub is a provider that declares a fixed capability set.
type capableStub struct{ caps agent.LLMCapabilities }

func (s *capableStub) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- agent.StreamEvent) (agent.LLMResult, error) {
	return agent.LLMResult{}, nil
}

func (s *capableStub) Capabilities() agent.LLMCapabilities { return s.caps }

func multimodal() *capableStub {
	return &capableStub{caps: agent.LLMCapabilities{ImageInput: true, StructuredOutput: true}}
}

func TestRouterCapabilitiesIntersectsMembers(t *testing.T) {
	var called string
	textOnly := &capableStub{caps: agent.LLMCapabilities{StructuredOutput: true}}

	for _, tc := range []struct {
		name   string
		router *RouterProvider
		want   agent.LLMCapabilities
	}{
		{
			name:   "all members multimodal",
			router: NewRouterProvider(multimodal()).AddRoute(Always(), multimodal()),
			want:   agent.LLMCapabilities{ImageInput: true, StructuredOutput: true},
		},
		{
			name:   "one text-only route drops image input",
			router: NewRouterProvider(multimodal()).AddRoute(Always(), textOnly),
			want:   agent.LLMCapabilities{StructuredOutput: true},
		},
		{
			name:   "text-only fallback drops image input",
			router: NewRouterProvider(textOnly).AddRoute(Always(), multimodal()),
			want:   agent.LLMCapabilities{StructuredOutput: true},
		},
		{
			// newStub makes no claim, so the intersection must collapse
			// rather than inherit the declaring member's answer.
			name:   "undeclared member collapses the report",
			router: NewRouterProvider(multimodal()).AddRoute(Always(), newStub("quiet", &called)),
			want:   agent.LLMCapabilities{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.router.Capabilities(); got != tc.want {
				t.Fatalf("Capabilities() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A router with no routes answers for its fallback alone.
func TestRouterCapabilitiesWithNoRoutesUsesFallback(t *testing.T) {
	r := NewRouterProvider(multimodal())
	want := agent.LLMCapabilities{ImageInput: true, StructuredOutput: true}
	if got := r.Capabilities(); got != want {
		t.Fatalf("Capabilities() = %+v, want %+v", got, want)
	}
}
