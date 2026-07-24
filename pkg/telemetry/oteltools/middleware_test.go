package oteltools_test

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

	"github.com/hung12ct/gopheragent/pkg/telemetry/oteltools"
	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// fakeTool is a controllable tools.Tool stub.
type fakeTool struct {
	name string
	res  tools.Result
	err  error
}

func (f *fakeTool) Descriptor() tools.ToolDescriptor { return tools.ToolDescriptor{Name: f.name} }
func (f *fakeTool) Execute(_ context.Context, _ string) (tools.Result, error) {
	return f.res, f.err
}

func TestInstrument_IdentityWhenNoProviders(t *testing.T) {
	base := &fakeTool{name: "noop"}
	got := oteltools.Instrument()(base)
	if got != tools.Tool(base) {
		t.Fatalf("expected identity middleware to return the same tool")
	}
}

func TestInstrument_SpanAndDurationOnSuccess(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	tool := oteltools.Instrument(
		oteltools.WithTracer(tp.Tracer("test")), oteltools.WithMeter(mp.Meter("test")),
	)(&fakeTool{name: "search", res: tools.Result{Text: "ok"}})

	ctx := tools.WithToolCallID(context.Background(), "call-42")
	if _, err := tool.Execute(ctx, "{}"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 1 || spans[0].Name() != "execute_tool search" {
		t.Fatalf("unexpected spans: %+v", spans)
	}
	if !hasStrAttr(spans[0].Attributes(), semconv.GenAIToolCallID, "call-42") {
		t.Fatalf("missing tool_call_id attribute: %v", spans[0].Attributes())
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !hasMetric(rm, semconv.MetricToolDuration) {
		t.Fatalf("missing duration metric")
	}
}

func TestInstrument_ErrorRecordsStatusAndCounter(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	boom := errors.New("tool boom")
	tool := oteltools.Instrument(
		oteltools.WithTracer(tp.Tracer("test")), oteltools.WithMeter(mp.Meter("test")),
	)(&fakeTool{name: "bad", err: boom})

	if _, err := tool.Execute(context.Background(), "{}"); !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	if spans := sr.Ended(); len(spans) != 1 || spans[0].Status().Code != codes.Error {
		t.Fatalf("expected 1 error span, got %+v", spans)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !hasMetric(rm, semconv.MetricToolErrors) {
		t.Fatalf("missing error counter metric")
	}
}

func hasStrAttr(attrs []attribute.KeyValue, key attribute.Key, want string) bool {
	for _, a := range attrs {
		if a.Key == key && a.Value.AsString() == want {
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
