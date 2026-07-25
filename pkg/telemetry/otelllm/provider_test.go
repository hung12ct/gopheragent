package otelllm_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/telemetry/otelllm"
	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// fakeProvider is a controllable agent.LLMProvider stub.
type fakeProvider struct {
	res agent.LLMResult
	err error
}

func (f *fakeProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- agent.StreamEvent) (agent.LLMResult, error) {
	return f.res, f.err
}

func newRecorders(t *testing.T) (*tracetest.SpanRecorder, *sdkmetric.ManualReader, *sdktrace.TracerProvider, *sdkmetric.MeterProvider) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return sr, reader, tp, mp
}

func TestNewProvider_PassthroughWhenNoProviders(t *testing.T) {
	f := &fakeProvider{}
	got := otelllm.NewProvider(f)
	if got != agent.LLMProvider(f) {
		t.Fatalf("expected passthrough to return the same provider, got a wrapper")
	}
}

func TestGenerateStream_SpanAndMetricsOnSuccess(t *testing.T) {
	sr, reader, tp, mp := newRecorders(t)
	f := &fakeProvider{res: agent.LLMResult{Usage: agent.TokenUsage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20}}}
	p := otelllm.NewProvider(f,
		otelllm.WithSystem("anthropic"), otelllm.WithModel("claude-test"),
		otelllm.WithTracer(tp.Tracer("test")), otelllm.WithMeter(mp.Meter("test")),
	)

	if _, err := p.GenerateStream(context.Background(), nil, nil, nil); err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	span := spans[0]
	if span.Name() != "chat claude-test" {
		t.Fatalf("span name = %q", span.Name())
	}
	if span.Status().Code != codes.Ok {
		t.Fatalf("span status = %v, want Ok", span.Status().Code)
	}
	if !hasIntAttr(span.Attributes(), semconv.GenAIUsageInputTokens, 12) {
		t.Fatalf("missing input token attribute: %v", span.Attributes())
	}
	if !hasIntAttr(span.Attributes(), semconv.GenAIUsageOutputTokens, 8) {
		t.Fatalf("missing output token attribute: %v", span.Attributes())
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !hasMetric(rm, semconv.MetricLLMDuration) {
		t.Fatalf("missing duration metric %q", semconv.MetricLLMDuration)
	}
	if !hasMetric(rm, semconv.MetricLLMTokenUsage) {
		t.Fatalf("missing token metric %q", semconv.MetricLLMTokenUsage)
	}
}

func TestGenerateStream_ErrorSetsSpanStatus(t *testing.T) {
	sr, _, tp, _ := newRecorders(t)
	sentinel := errors.New("provider boom")
	p := otelllm.NewProvider(&fakeProvider{err: sentinel},
		otelllm.WithModel("m"), otelllm.WithTracer(tp.Tracer("test")))

	if _, err := p.GenerateStream(context.Background(), nil, nil, nil); !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Fatalf("span status = %v, want Error", spans[0].Status().Code)
	}
}

func hasIntAttr(attrs []attribute.KeyValue, key attribute.Key, want int64) bool {
	for _, a := range attrs {
		if a.Key == key && a.Value.AsInt64() == want {
			return true
		}
	}
	return false
}

func hasMetric(rm metricdata.ResourceMetrics, name string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}
