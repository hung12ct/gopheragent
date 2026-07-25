// Package otelllm provides an OpenTelemetry instrumenting decorator for any
// agent.LLMProvider. It wraps GenerateStream with a span and records call
// latency plus prompt/completion token metrics, without touching the concrete
// provider packages (pkg/llm/*).
//
// It imports only the OpenTelemetry API (trace + metric), never the SDK, so it
// is a no-op — and returns the wrapped provider unchanged — when no Tracer or
// Meter is supplied.
//
// Usage:
//
//	llm := otelllm.NewProvider(p,
//	    otelllm.WithSystem("anthropic"), otelllm.WithModel(model),
//	    otelllm.WithTracer(tel.Tracer), otelllm.WithMeter(tel.Meter))
//	loop := agent.New(sessions, registry, llm)
//
// For a RouterProvider, wrap each backing provider before registering it so the
// gen_ai.request.model attribute is correct per route.
package otelllm

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// Option configures NewProvider.
type Option func(*config)

type config struct {
	tracer trace.Tracer
	meter  metric.Meter
	system string
	model  string
}

// WithSystem sets the gen_ai.system attribute (model vendor), e.g. "anthropic".
func WithSystem(system string) Option {
	return func(c *config) { c.system = system }
}

// WithModel sets the gen_ai.request.model attribute, e.g. "claude-opus-4-8".
func WithModel(model string) Option {
	return func(c *config) { c.model = model }
}

// WithTracer supplies the Tracer used to open per-call spans. Without it, no
// spans are produced.
func WithTracer(t trace.Tracer) Option {
	return func(c *config) { c.tracer = t }
}

// WithMeter supplies the Meter used to record latency and token metrics.
// Without it, no metrics are produced.
func WithMeter(m metric.Meter) Option {
	return func(c *config) { c.meter = m }
}

// NewProvider wraps next with tracing and metrics. When neither a Tracer nor a
// Meter is supplied it returns next unchanged, so wiring the decorator without
// providers costs nothing on the hot path.
func NewProvider(next agent.LLMProvider, opts ...Option) agent.LLMProvider {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.tracer == nil && cfg.meter == nil {
		return next
	}
	p := &instrumentedProvider{
		next:   next,
		tracer: cfg.tracer,
		system: cfg.system,
		model:  cfg.model,
	}
	p.initInstruments(cfg.meter)
	return p
}

// instrumentedProvider decorates an agent.LLMProvider with OTel spans/metrics.
type instrumentedProvider struct {
	next   agent.LLMProvider
	tracer trace.Tracer
	system string
	model  string

	duration metric.Float64Histogram
	tokens   metric.Int64Counter
}

// initInstruments builds the metric instruments once. If meter is nil or an
// instrument cannot be created, the corresponding field stays nil and recording
// is skipped — the decorator still traces.
func (p *instrumentedProvider) initInstruments(meter metric.Meter) {
	if meter == nil {
		return
	}
	if h, err := meter.Float64Histogram(
		semconv.MetricLLMDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of LLM GenerateStream calls."),
		metric.WithExplicitBucketBoundaries(semconv.DurationBucketsSeconds...),
	); err == nil {
		p.duration = h
	}
	if c, err := meter.Int64Counter(
		semconv.MetricLLMTokenUsage,
		metric.WithUnit("{token}"),
		metric.WithDescription("Tokens consumed by LLM calls, split by gen_ai.token.type."),
	); err == nil {
		p.tokens = c
	}
}

// GenerateStream instruments the wrapped call: it opens a "chat <model>" span,
// records latency, sets token attributes/metrics from the result, and marks the
// span as failed on error.
func (p *instrumentedProvider) GenerateStream(
	ctx context.Context,
	memory []history.Message,
	availableTools *tools.Registry,
	streamChan chan<- agent.StreamEvent,
) (agent.LLMResult, error) {
	ctx, span := p.startSpan(ctx)
	start := time.Now()

	res, err := p.next.GenerateStream(ctx, memory, availableTools, streamChan)

	p.recordDuration(ctx, time.Since(start))
	p.recordResult(ctx, span, res, err)
	if span != nil {
		span.End()
	}
	return res, err
}

func (p *instrumentedProvider) startSpan(ctx context.Context) (context.Context, trace.Span) {
	if p.tracer == nil {
		return ctx, nil
	}
	return p.tracer.Start(ctx, "chat "+p.model,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.GenAIOperationName.String(semconv.OperationChat),
			semconv.GenAISystem.String(p.system),
			semconv.GenAIRequestModel.String(p.model),
		),
	)
}

func (p *instrumentedProvider) recordDuration(ctx context.Context, d time.Duration) {
	if p.duration == nil {
		return
	}
	p.duration.Record(ctx, d.Seconds(), metric.WithAttributes(
		semconv.GenAISystem.String(p.system),
		semconv.GenAIRequestModel.String(p.model),
	))
}

// recordResult sets usage attributes on the span and emits token counters, then
// marks the span status from err.
func (p *instrumentedProvider) recordResult(ctx context.Context, span trace.Span, res agent.LLMResult, err error) {
	if span != nil {
		span.SetAttributes(
			semconv.GenAIUsageInputTokens.Int(res.Usage.PromptTokens),
			semconv.GenAIUsageOutputTokens.Int(res.Usage.CompletionTokens),
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
	}
	if p.tokens != nil {
		// len == cap == 2, so each append below reallocates a fresh backing
		// array — the two token-type attribute sets never alias.
		base := []attribute.KeyValue{
			semconv.GenAISystem.String(p.system),
			semconv.GenAIRequestModel.String(p.model),
		}
		if res.Usage.PromptTokens > 0 {
			p.tokens.Add(ctx, int64(res.Usage.PromptTokens),
				metric.WithAttributes(append(base, semconv.GenAITokenType.String(semconv.TokenTypeInput))...))
		}
		if res.Usage.CompletionTokens > 0 {
			p.tokens.Add(ctx, int64(res.Usage.CompletionTokens),
				metric.WithAttributes(append(base, semconv.GenAITokenType.String(semconv.TokenTypeOutput))...))
		}
	}
}
