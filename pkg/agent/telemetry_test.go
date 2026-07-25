package agent

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func hasStrAttr(attrs []attribute.KeyValue, key attribute.Key, want string) bool {
	for _, a := range attrs {
		if a.Key == key && a.Value.AsString() == want {
			return true
		}
	}
	return false
}

// childSpanProvider starts a child span from the ctx it receives — emulating
// what the otelllm decorator does — so the test can assert the iteration span
// parents it.
type childSpanProvider struct {
	tracer   trace.Tracer
	childCtx trace.SpanContext
}

func (p *childSpanProvider) GenerateStream(ctx context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	_, span := p.tracer.Start(ctx, "child-llm")
	p.childCtx = span.SpanContext()
	span.End()
	ch <- Event(ContentEvent{Text: "done"})
	return LLMResult{Content: "done"}, nil
}

func TestIterationSpan_ParentsChildSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	tr := tp.Tracer("test")

	provider := &childSpanProvider{tracer: tr}
	loop := New(history.NewInMemSessionManager("sys"), tools.NewRegistry(), provider, WithTracer(tr))

	if _, err := loop.RunIteration(context.Background(), "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}

	var iter, child sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		switch s.Name() {
		case "agent.iteration":
			iter = s
		case "child-llm":
			child = s
		}
	}
	if iter == nil || child == nil {
		t.Fatalf("missing spans: iter=%v child=%v", iter != nil, child != nil)
	}
	if child.Parent().SpanID() != iter.SpanContext().SpanID() {
		t.Fatalf("child not parented by iteration span: parent=%v iter=%v",
			child.Parent().SpanID(), iter.SpanContext().SpanID())
	}
}

// cancelProvider returns an error wrapping context.Canceled, emulating a cancel
// that lands mid-stream.
type cancelProvider struct{}

func (cancelProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- StreamEvent) (LLMResult, error) {
	return LLMResult{}, fmt.Errorf("provider stream: %w", context.Canceled)
}

func TestRunSpan_SingleTracePerTurn(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	tr := tp.Tracer("test")

	provider := &childSpanProvider{tracer: tr}
	loop := New(history.NewInMemSessionManager("sys"), tools.NewRegistry(), provider, WithTracer(tr))

	if _, err := loop.RunIteration(context.Background(), "conv-abc", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}

	spans := sr.Ended()
	if len(spans) < 3 {
		t.Fatalf("expected at least run+iteration+child spans, got %d", len(spans))
	}

	// Every span in the turn must share one trace ID.
	var run sdktrace.ReadOnlySpan
	traceID := spans[0].SpanContext().TraceID()
	for _, s := range spans {
		if s.SpanContext().TraceID() != traceID {
			t.Fatalf("span %q has a different trace ID — turn is not a single trace", s.Name())
		}
		if s.Name() == "agent.run" {
			run = s
		}
	}
	if run == nil {
		t.Fatal("no agent.run root span recorded")
	}
	if run.Parent().IsValid() {
		t.Fatalf("agent.run should be the trace root, but has a parent")
	}
	if !hasStrAttr(run.Attributes(), semconv.SessionKey, "conv-abc") {
		t.Fatalf("agent.run missing session.key attribute: %v", run.Attributes())
	}
}

func TestRunSpan_NestsUnderCallerSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	tr := tp.Tracer("test")

	// Caller wraps the turn in its own span to own the trace ID.
	ctx, outer := tr.Start(context.Background(), "conversation")
	wantTrace := outer.SpanContext().TraceID()

	loop := New(history.NewInMemSessionManager("sys"), tools.NewRegistry(), &childSpanProvider{tracer: tr}, WithTracer(tr))
	if _, err := loop.RunIteration(ctx, "conv-xyz", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	outer.End()

	for _, s := range sr.Ended() {
		if s.SpanContext().TraceID() != wantTrace {
			t.Fatalf("span %q escaped the caller's trace", s.Name())
		}
	}
}

func TestIterationSpan_CancelMarksError(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	loop := New(history.NewInMemSessionManager("sys"), tools.NewRegistry(), cancelProvider{}, WithTracer(tp.Tracer("test")))

	if _, err := loop.RunIteration(context.Background(), "s1", "hi"); err == nil {
		t.Fatal("expected an error from the cancelled iteration")
	}

	var iter sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "agent.iteration" {
			iter = s
		}
	}
	if iter == nil {
		t.Fatal("no iteration span recorded")
	}
	if iter.Status().Code != codes.Error {
		t.Fatalf("iteration span status = %v, want Error (cancel should not read as Ok)", iter.Status().Code)
	}
}

func TestConfigure_AppliesOptionsAndChains(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	loop := New(history.NewInMemSessionManager("sys"), tools.NewRegistry(), NewMockProvider())

	got := loop.Configure(WithMaxIters(3), nil, WithTracer(tp.Tracer("test")), WithMaxIters(9))
	if got != loop {
		t.Fatal("Configure should return the same loop for chaining")
	}
	if loop.MaxIters != 9 {
		t.Fatalf("MaxIters = %d, want 9 (later option should win)", loop.MaxIters)
	}
	if loop.tracer == nil {
		t.Fatal("WithTracer via Configure did not set the tracer")
	}
}

func TestStartIterationSpan_ZeroAllocWhenDisabled(t *testing.T) {
	al := &AgentLoop{} // no tracer, no meter
	allocs := testing.AllocsPerRun(1000, func() {
		ctx, end := al.startIterationSpan(context.Background(), "s", 0)
		end(nil)
		_ = ctx
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocations when telemetry disabled, got %v", allocs)
	}
}
