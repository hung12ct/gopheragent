package builder

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

type answerLLM struct{}

func (answerLLM) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- agent.StreamEvent) (agent.LLMResult, error) {
	return agent.LLMResult{Content: "ok"}, nil
}

func TestBuild_StampsYAMLNameOnRunSpan(t *testing.T) {
	p := writeYAML(t, `
agent:
  name: "Test Agent"
  system_prompt: "You are helpful."
`)
	loop, _, _, err := BuildFromYAML(p, NewGlobalCatalog(), answerLLM{}, nil)
	if err != nil {
		t.Fatalf("BuildFromYAML: %v", err)
	}
	sr := tracetest.NewSpanRecorder()
	loop.Configure(agent.WithTracer(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)).Tracer("t")))
	if _, err := loop.RunIteration(context.Background(), "s", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	for _, s := range sr.Ended() {
		if s.Parent().IsValid() {
			continue
		}
		for _, a := range s.Attributes() {
			if a.Key == semconv.GenAIAgentName && a.Value.AsString() == "Test Agent" {
				return
			}
		}
		t.Fatalf("run span lacks gen_ai.agent.name=Test Agent: %v", s.Attributes())
	}
	t.Fatal("no run span recorded")
}
