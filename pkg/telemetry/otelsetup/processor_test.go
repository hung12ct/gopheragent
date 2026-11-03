package otelsetup

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/hung12ct/gopheragent/pkg/telemetry/spanattr"
)

func attrMap(kvs []attribute.KeyValue) map[attribute.Key]string {
	m := make(map[attribute.Key]string, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value.Emit()
	}
	return m
}

func TestAttrProcessor_Precedence(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(attrProcessor{baggageKeys: []string{"team.id", "tenant.id"}}),
		sdktrace.WithSpanProcessor(sr),
	)
	bag, err := baggage.Parse("team.id=core,tenant.id=from-baggage,secret=leak")
	if err != nil {
		t.Fatal(err)
	}
	ctx := baggage.ContextWithBaggage(context.Background(), bag)
	ctx = spanattr.With(ctx,
		attribute.String("tenant.id", "acme"),         // beats baggage
		attribute.String("gen_ai.tool.name", "spoof"), // loses to the instrumentation
	)
	_, span := tp.Tracer("t").Start(ctx, "s", trace.WithAttributes(attribute.String("gen_ai.tool.name", "echo")))
	span.End()

	got := attrMap(sr.Ended()[0].Attributes())
	want := map[attribute.Key]string{"team.id": "core", "tenant.id": "acme", "gen_ai.tool.name": "echo"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if _, ok := got["secret"]; ok {
		t.Fatal("non-allowlisted baggage member copied onto span")
	}
}

func TestAttrProcessor_NoAttrsNoAlloc(t *testing.T) {
	p := attrProcessor{}
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	_, span := tp.Tracer("t").Start(context.Background(), "s")
	rw := span.(sdktrace.ReadWriteSpan)
	ctx := context.Background()
	if allocs := testing.AllocsPerRun(1000, func() { p.OnStart(ctx, rw) }); allocs != 0 {
		t.Fatalf("OnStart with nothing to stamp allocated %v times", allocs)
	}
	span.End()
}
