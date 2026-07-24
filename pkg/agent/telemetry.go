package agent

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
)

// noopEndSpan is the shared end function returned by startIterationSpan when
// telemetry is disabled. Sharing one value avoids allocating a closure per
// iteration on the hot path.
var noopEndSpan = func(error) {}

// WithTracer enables OpenTelemetry tracing of the ReAct loop. Each iteration
// opens an "agent.iteration" span; because that span rides on the ctx threaded
// into the LLM call and tool execution, spans produced by the otelllm decorator
// and the oteltools middleware nest as its children. nil (the default) disables
// tracing at zero hot-path cost — no span is created and ctx is untouched.
func WithTracer(t trace.Tracer) Option {
	return func(al *AgentLoop) { al.tracer = t }
}

// WithMeter enables OpenTelemetry metrics for the loop. It records a
// per-iteration latency histogram (gopheragent.agent.iteration.duration). The
// instrument is built once here so no allocation happens per iteration. nil
// (the default) disables metrics.
func WithMeter(m metric.Meter) Option {
	return func(al *AgentLoop) {
		if m == nil {
			al.iterHist = nil
			return
		}
		if h, err := m.Float64Histogram(
			semconv.MetricIterationDuration,
			metric.WithUnit("s"),
			metric.WithDescription("Duration of a single ReAct iteration."),
		); err == nil {
			al.iterHist = h
		}
	}
}

// startIterationSpan opens the per-iteration span and returns the child ctx plus
// an end function that closes the span and records the iteration-duration metric.
// When no tracer and no meter is configured it returns the original ctx and a
// no-op end function, performing no allocation — this keeps the loop's hot path
// free when telemetry is off.
func (al *AgentLoop) startIterationSpan(ctx context.Context, sessionKey string, iteration int) (context.Context, func(err error)) {
	if al.tracer == nil && al.iterHist == nil {
		return ctx, noopEndSpan
	}

	var span trace.Span
	if al.tracer != nil {
		ctx, span = al.tracer.Start(ctx, "agent.iteration",
			trace.WithAttributes(
				semconv.SessionKey.String(sessionKey),
				semconv.Iteration.Int(iteration),
			),
		)
	}
	start := time.Now()

	return ctx, func(err error) {
		if al.iterHist != nil {
			al.iterHist.Record(ctx, time.Since(start).Seconds(),
				metric.WithAttributes(semconv.SessionKey.String(sessionKey)))
		}
		if span != nil {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			} else {
				span.SetStatus(codes.Ok, "")
			}
			span.End()
		}
	}
}
