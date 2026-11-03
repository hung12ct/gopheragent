package oteltools_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/hung12ct/gopheragent/pkg/telemetry/oteltools"
	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

type textTool struct{}

func (textTool) Descriptor() tools.ToolDescriptor { return tools.ToolDescriptor{Name: "t"} }
func (textTool) Execute(context.Context, string) (tools.Result, error) {
	return tools.Text("tool-output"), nil
}

func toolSpanAttrs(t *testing.T, opts ...oteltools.Option) map[attribute.Key]string {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	tool := tools.Chain(textTool{}, oteltools.Instrument(append(opts, oteltools.WithTracer(tp.Tracer("t")))...))
	if _, err := tool.Execute(context.Background(), `{"q":"secret"}`); err != nil {
		t.Fatal(err)
	}
	out := map[attribute.Key]string{}
	for _, a := range sr.Ended()[0].Attributes() {
		out[a.Key] = a.Value.Emit()
	}
	return out
}

func TestToolContentCapture(t *testing.T) {
	t.Setenv(semconv.EnvCaptureContent, "")
	if _, ok := toolSpanAttrs(t)[semconv.GenAIToolCallArguments]; ok {
		t.Fatal("tool arguments captured without opt-in")
	}
	got := toolSpanAttrs(t, oteltools.WithContentCapture(true))
	if got[semconv.GenAIToolCallArguments] != `{"q":"secret"}` || got[semconv.GenAIToolCallResult] != "tool-output" {
		t.Fatalf("captured = %v", got)
	}
}
