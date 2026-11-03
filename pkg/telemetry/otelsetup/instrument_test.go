package otelsetup_test

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/telemetry/otelllm"
	"github.com/hung12ct/gopheragent/pkg/telemetry/otelsetup"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// echoLoop builds an uninstrumented loop whose LLM calls echo once, then answers.
func echoLoop() *agent.AgentLoop {
	reg := tools.NewRegistry()
	tools.RegisterFunc(reg, "echo", "echo text", func(_ context.Context, a echoArgs) (tools.Result, error) {
		return tools.Text(a.Text), nil
	})
	return agent.New(history.NewInMemSessionManager("sys"), reg, &onceToolLLM{})
}

type onceToolLLM struct{ called bool }

func (o *onceToolLLM) GenerateStream(context.Context, []history.Message, *tools.Registry, chan<- agent.StreamEvent) (agent.LLMResult, error) {
	if o.called {
		return agent.LLMResult{Content: "done"}, nil
	}
	o.called = true
	return agent.LLMResult{ToolCalls: []agent.PendingToolCall{{ID: "c1", Name: "echo", ArgsJSON: `{"text":"hi"}`}}}, nil
}

func TestInstrument_DisabledChangesNothing(t *testing.T) {
	clearOTelEnv(t)
	tel, shutdown, err := otelsetup.Setup(context.Background(), otelsetup.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	loop := echoLoop()
	llm := loop.LLM
	echo, _ := loop.Tools.Get("echo")
	if tel.Instrument(loop) != loop || loop.LLM != llm {
		t.Fatal("disabled Instrument must leave the loop's LLM untouched")
	}
	if after, _ := loop.Tools.Get("echo"); after != echo {
		t.Fatal("disabled Instrument must leave tools unwrapped")
	}
	var nilProviders *otelsetup.Providers
	if nilProviders.Instrument(loop) != loop {
		t.Fatal("nil Providers must be a no-op")
	}
}

// TestInstrument_OneCallMatchesManualWiring: one Instrument call (twice, to
// prove idempotence) yields the full run → iteration → chat → execute_tool
// tree over real OTLP, each span exactly once.
func TestInstrument_OneCallMatchesManualWiring(t *testing.T) {
	clearOTelEnv(t)
	prevTP, prevMP, prevProp := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
	})
	r := &receiver{}
	tel, shutdown, err := otelsetup.Setup(context.Background(), otelsetup.Config{ServiceName: "e2e", Endpoint: startGRPC(t, r), Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	loop := echoLoop()
	tel.Instrument(loop, otelllm.WithSystem("fake"), otelllm.WithModel("m"))
	tel.Instrument(loop, otelllm.WithSystem("fake"), otelllm.WithModel("m"))

	if _, err := loop.RunIteration(context.Background(), "s", "go"); err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, s := range r.spans {
		counts[s.GetName()]++
	}
	want := map[string]int{"agent.run": 1, "agent.iteration": 2, "chat m": 2, "execute_tool echo": 1}
	for name, n := range want {
		if counts[name] != n {
			t.Fatalf("span %q x%d, want x%d (all: %v)", name, counts[name], n, counts)
		}
	}
	if len(r.metrics) == 0 {
		t.Fatal("no metrics exported")
	}
}

// TestSetup_EnvOnlyEndpointExports covers the env-only path both ways an
// operator writes it: "host:port" (which the exporter alone can't parse) and
// a URL.
func TestSetup_EnvOnlyEndpointExports(t *testing.T) {
	for _, form := range []string{"%s", "http://%s"} {
		t.Run(form, func(t *testing.T) {
			clearOTelEnv(t)
			prevTP, prevMP := otel.GetTracerProvider(), otel.GetMeterProvider()
			t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetMeterProvider(prevMP) })
			r := &receiver{}
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", fmt.Sprintf(form, startGRPC(t, r)))
			t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
			tel, shutdown, err := otelsetup.Setup(context.Background(), otelsetup.Config{ServiceName: "e2e"})
			if err != nil {
				t.Fatal(err)
			}
			loop := tel.Instrument(echoLoop())
			_, _ = loop.RunIteration(context.Background(), "s", "go")
			if err := shutdown(context.Background()); err != nil {
				t.Fatalf("export with env endpoint %q failed: %v", form, err)
			}
			if len(r.spans) == 0 {
				t.Fatalf("no spans exported with OTEL_EXPORTER_OTLP_ENDPOINT in form %q", form)
			}
		})
	}
}
