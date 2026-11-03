package otelllm_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/telemetry/otelllm"
	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
)

// runChatOnce drives one instrumented call and returns the chat span's
// attributes plus the token-usage data points.
func runChatOnce(t *testing.T) ([]attribute.KeyValue, []metricdata.DataPoint[int64]) {
	t.Helper()
	sr, reader, tp, mp := newRecorders(t)
	f := &fakeProvider{res: agent.LLMResult{Usage: agent.TokenUsage{PromptTokens: 3, CompletionTokens: 2}}}
	p := otelllm.NewProvider(f,
		otelllm.WithSystem("anthropic"), otelllm.WithModel("m"),
		otelllm.WithTracer(tp.Tracer("test")), otelllm.WithMeter(mp.Meter("test")),
	)
	if _, err := p.GenerateStream(context.Background(), nil, nil, nil); err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var points []metricdata.DataPoint[int64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if s, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == semconv.MetricLLMTokenUsage {
				points = append(points, s.DataPoints...)
			}
		}
	}
	if len(sr.Ended()) != 1 || len(points) != 2 {
		t.Fatalf("spans=%d token points=%d, want 1 and 2", len(sr.Ended()), len(points))
	}
	return sr.Ended()[0].Attributes(), points
}

func hasKey(attrs []attribute.KeyValue, k attribute.Key) bool {
	for _, a := range attrs {
		if a.Key == k {
			return true
		}
	}
	return false
}

func TestSchemaLegacy_DualEmitsVendorOnSpanKeepsMetricLabel(t *testing.T) {
	t.Setenv(semconv.EnvStabilityOptIn, "")
	spanAttrs, points := runChatOnce(t)
	if !hasKey(spanAttrs, semconv.GenAISystem) || !hasKey(spanAttrs, semconv.GenAIProviderName) {
		t.Fatalf("legacy span should carry both vendor keys: %v", spanAttrs)
	}
	for _, dp := range points {
		if _, ok := dp.Attributes.Value(semconv.GenAISystem); !ok {
			t.Fatalf("legacy metric must keep gen_ai.system label: %v", dp.Attributes.ToSlice())
		}
		if _, ok := dp.Attributes.Value(semconv.GenAIProviderName); ok {
			t.Fatalf("legacy metric must not add gen_ai.provider.name (splits series)")
		}
	}
}

func TestSchemaLatest_UsesProviderNameOnly(t *testing.T) {
	t.Setenv(semconv.EnvStabilityOptIn, "http,"+semconv.OptInGenAILatest)
	spanAttrs, points := runChatOnce(t)
	if hasKey(spanAttrs, semconv.GenAISystem) || !hasKey(spanAttrs, semconv.GenAIProviderName) {
		t.Fatalf("latest span should carry only gen_ai.provider.name: %v", spanAttrs)
	}
	for _, dp := range points {
		if _, ok := dp.Attributes.Value(semconv.GenAISystem); ok {
			t.Fatalf("latest metric must not carry gen_ai.system: %v", dp.Attributes.ToSlice())
		}
		if _, ok := dp.Attributes.Value(semconv.GenAITokenType); !ok {
			t.Fatalf("token metric lost gen_ai.token.type: %v", dp.Attributes.ToSlice())
		}
	}
}
