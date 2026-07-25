// Package oteltools provides an OpenTelemetry tools.Middleware that traces each
// tool execution and records latency and error metrics. It wraps tools through
// the existing tools.Chain mechanism, so pkg/tools itself is never modified.
//
// It imports only the OpenTelemetry API (trace + metric), never the SDK. When
// no Tracer or Meter is supplied it returns an identity middleware, so wiring it
// without providers leaves the tool unwrapped and costs nothing on the hot path.
//
// Usage:
//
//	reg.Register(tools.Chain(myTool,
//	    oteltools.Instrument(oteltools.WithTracer(tel.Tracer), oteltools.WithMeter(tel.Meter))))
//
// When wired under an agent loop that opens iteration spans (agent.WithTracer),
// these tool spans nest as children of the iteration span automatically via ctx.
package oteltools

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// Option configures Instrument.
type Option func(*config)

type config struct {
	tracer trace.Tracer
	meter  metric.Meter
}

// WithTracer supplies the Tracer used to open per-Execute spans.
func WithTracer(t trace.Tracer) Option {
	return func(c *config) { c.tracer = t }
}

// WithMeter supplies the Meter used to record execution latency and errors.
func WithMeter(m metric.Meter) Option {
	return func(c *config) { c.meter = m }
}

// Instrument returns a tools.Middleware that opens an "execute_tool <name>" span
// and records duration + error metrics for each call. With neither a Tracer nor
// a Meter it returns an identity middleware (the tool is passed through
// unwrapped).
func Instrument(opts ...Option) tools.Middleware {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.tracer == nil && cfg.meter == nil {
		return func(next tools.Tool) tools.Tool { return next }
	}

	var duration metric.Float64Histogram
	var errCounter metric.Int64Counter
	if cfg.meter != nil {
		duration, _ = cfg.meter.Float64Histogram(
			semconv.MetricToolDuration,
			metric.WithUnit("s"),
			metric.WithDescription("Duration of tool executions."),
		)
		errCounter, _ = cfg.meter.Int64Counter(
			semconv.MetricToolErrors,
			metric.WithUnit("{error}"),
			metric.WithDescription("Count of failed tool executions."),
		)
	}

	return func(next tools.Tool) tools.Tool {
		return &instrumentedTool{
			Tool:     next,
			name:     next.Descriptor().Name,
			tracer:   cfg.tracer,
			duration: duration,
			errors:   errCounter,
		}
	}
}

// instrumentedTool wraps a tools.Tool, delegating Descriptor and instrumenting
// Execute.
type instrumentedTool struct {
	tools.Tool
	name     string
	tracer   trace.Tracer
	duration metric.Float64Histogram
	errors   metric.Int64Counter
}

func (t *instrumentedTool) Execute(ctx context.Context, argsJSON string) (tools.Result, error) {
	attrs := []attribute.KeyValue{
		semconv.GenAIToolName.String(t.name),
		semconv.GenAIToolCallID.String(tools.ToolCallIDFromContext(ctx)),
	}
	var span trace.Span
	if t.tracer != nil {
		ctx, span = t.tracer.Start(ctx, "execute_tool "+t.name,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attrs...),
		)
	}

	start := time.Now()
	result, err := t.Tool.Execute(ctx, argsJSON)
	elapsed := time.Since(start)

	if t.duration != nil {
		t.duration.Record(ctx, elapsed.Seconds(),
			metric.WithAttributes(semconv.GenAIToolName.String(t.name)))
	}
	if err != nil && t.errors != nil {
		t.errors.Add(ctx, 1, metric.WithAttributes(semconv.GenAIToolName.String(t.name)))
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
	return result, err
}
