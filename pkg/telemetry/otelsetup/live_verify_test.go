package otelsetup_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/telemetry/otelllm"
	"github.com/hung12ct/gopheragent/pkg/telemetry/otelsetup"
	"github.com/hung12ct/gopheragent/pkg/telemetry/oteltools"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// scriptedProvider returns a scripted sequence of turns with token usage, so the
// live test produces a chat span + token metrics without a real LLM key.
type scriptedProvider struct {
	turns []agent.LLMResult
	i     int
}

func (p *scriptedProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- agent.StreamEvent) (agent.LLMResult, error) {
	time.Sleep(15 * time.Millisecond) // give the duration histogram a visible value
	if p.i >= len(p.turns) {
		return agent.LLMResult{Content: "done"}, nil
	}
	r := p.turns[p.i]
	p.i++
	if len(r.ToolCalls) == 0 {
		ch <- agent.Event(agent.ContentEvent{Text: r.Content})
	}
	return r, nil
}

// echoTool is a trivial tool so the loop emits an execute_tool span.
type echoTool struct{}

func (echoTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{Name: "echo", Description: "echoes its args"}
}
func (echoTool) Execute(_ context.Context, args string) (tools.Result, error) {
	time.Sleep(5 * time.Millisecond)
	return tools.Text("echo:" + args), nil
}

// TestLive_FullTraceExport drives the real instrumentation end to end and exports
// to an OTLP collector on localhost:4317. It is skipped unless OTEL_LIVE=1.
//
//	OTEL_LIVE=1 go test -run TestLive_FullTraceExport -count=1 ./pkg/telemetry/otelsetup/
//
// After it runs, query Tempo for { .gopheragent.session.key = "live-verify-001" }
// and Prometheus for gen_ai_client_operation_duration / *_token_usage.
func TestLive_FullTraceExport(t *testing.T) {
	if os.Getenv("OTEL_LIVE") == "" {
		t.Skip("set OTEL_LIVE=1 and run an OTLP collector on localhost:4317")
	}
	ctx := context.Background()

	tel, shutdown, err := otelsetup.Setup(ctx, otelsetup.Config{
		ServiceName: "gopheragent-live-verify",
		Endpoint:    "localhost:4317",
		Insecure:    true,
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	provider := otelllm.NewProvider(
		&scriptedProvider{turns: []agent.LLMResult{
			{ToolCalls: []agent.PendingToolCall{{ID: "c1", Name: "echo", ArgsJSON: `"hi"`}},
				Usage: agent.TokenUsage{PromptTokens: 40, CompletionTokens: 12, TotalTokens: 52}},
			{Content: "final answer",
				Usage: agent.TokenUsage{PromptTokens: 55, CompletionTokens: 20, TotalTokens: 75}},
		}},
		otelllm.WithSystem("scripted"), otelllm.WithModel("verify-model"),
		otelllm.WithTracer(tel.Tracer), otelllm.WithMeter(tel.Meter),
	)

	reg := tools.NewRegistry()
	reg.Register(tools.Chain(echoTool{},
		oteltools.Instrument(oteltools.WithTracer(tel.Tracer), oteltools.WithMeter(tel.Meter))))

	loop := agent.New(history.NewInMemSessionManager("sys"), reg, provider,
		agent.WithTracer(tel.Tracer), agent.WithMeter(tel.Meter))

	answer, err := loop.RunIteration(ctx, "live-verify-001", "call echo then answer")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if answer != "final answer" {
		t.Fatalf("answer = %q, want %q", answer, "final answer")
	}

	// Flush the batch span processor + periodic metric reader to the collector.
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	t.Log("exported trace for session live-verify-001 (run + iteration + chat + execute_tool)")
}
