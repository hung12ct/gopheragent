package agent

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

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
