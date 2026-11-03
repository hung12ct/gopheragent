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
	tracer     trace.Tracer
	meter      metric.Meter
	system     string
	model      string
	capture    *bool
	contentMax int
}

// WithContentCapture records prompts and completions on the chat span as
// gen_ai.system_instructions / gen_ai.input.messages / gen_ai.output.messages.
// It overrides OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT. Off by
// default: content is PII and the largest telemetry cost driver. Media bytes
// are never captured.
func WithContentCapture(enabled bool) Option {
	return func(c *config) { c.capture = &enabled }
}

// WithContentMaxBytes caps each captured text field (default
// semconv.DefaultContentMaxBytes); the whole input payload is bounded to a
// fixed multiple of it, keeping the newest messages.
func WithContentMaxBytes(n int) Option {
	return func(c *config) { c.contentMax = n }
}

// WithSystem sets the model vendor, e.g. "anthropic". It is emitted as
// gen_ai.provider.name (and gen_ai.system under semconv.SchemaLegacy).
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
	cfg := &config{contentMax: semconv.DefaultContentMaxBytes}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.tracer == nil && cfg.meter == nil {
		return next
	}
	switch next.(type) {
	case *instrumentedProvider, *capableProvider:
		return next // already instrumented: wrapping again would double every span
	}
	capture := semconv.CaptureContentFromEnv()
	if cfg.capture != nil {
		capture = *cfg.capture
	}
	p := &instrumentedProvider{
		next:       next,
		tracer:     cfg.tracer,
		model:      cfg.model,
		capture:    capture,
		contentMax: cfg.contentMax,
	}
	p.initAttributes(cfg.system, semconv.SchemaFromEnv())
	p.initInstruments(cfg.meter)
	if c, ok := next.(agent.CapabilityProvider); ok {
		return &capableProvider{instrumentedProvider: p, caps: c}
	}
	return p
}

// capableProvider is the variant returned when the wrapped provider reports
// capabilities. Selecting the type at construction — rather than always
// implementing Capabilities on instrumentedProvider — keeps "absent means
// unknown" intact: wrapping a provider that makes no claim must not turn it
// into a provider claiming it supports nothing.
type capableProvider struct {
	*instrumentedProvider
	caps agent.CapabilityProvider
}

// Capabilities forwards the wrapped provider's report so that adding tracing
// does not erase a capability the caller checks for.
func (p *capableProvider) Capabilities() agent.LLMCapabilities {
	return p.caps.Capabilities()
}

// instrumentedProvider decorates an agent.LLMProvider with OTel spans/metrics.
type instrumentedProvider struct {
	next       agent.LLMProvider
	tracer     trace.Tracer
	model      string
	capture    bool
	contentMax int

	// Attribute sets are fixed per provider, so they are built once in
	// initAttributes instead of per call: the metric options carry
	// pre-sorted attribute.Sets, keeping recording allocation-free.
	spanAttrs    []attribute.KeyValue
	durationOpt  metric.MeasurementOption
	inputTokOpt  metric.MeasurementOption
	outputTokOpt metric.MeasurementOption

	duration metric.Float64Histogram
	tokens   metric.Int64Counter
}

// initAttributes precomputes span attributes and metric attribute sets for the
// semconv schema. Legacy keeps gen_ai.system as the metric label (so existing
// series do not split) and stamps both vendor keys on spans; Latest uses only
// gen_ai.provider.name.
func (p *instrumentedProvider) initAttributes(system string, schema semconv.Schema) {
	vendor := semconv.GenAIProviderName.String(system)
	if schema == semconv.SchemaLegacy {
		vendor = semconv.GenAISystem.String(system)
	}
	model := semconv.GenAIRequestModel.String(p.model)

	p.spanAttrs = []attribute.KeyValue{
		semconv.GenAIOperationName.String(semconv.OperationChat),
		semconv.GenAIProviderName.String(system),
		model,
	}
	if schema == semconv.SchemaLegacy {
		p.spanAttrs = append(p.spanAttrs, semconv.GenAISystem.String(system))
	}

	metricSet := func(extra ...attribute.KeyValue) metric.MeasurementOption {
		kvs := append([]attribute.KeyValue{vendor, model}, extra...)
		return metric.WithAttributeSet(attribute.NewSet(kvs...))
	}
	p.durationOpt = metricSet()
	p.inputTokOpt = metricSet(semconv.GenAITokenType.String(semconv.TokenTypeInput))
	p.outputTokOpt = metricSet(semconv.GenAITokenType.String(semconv.TokenTypeOutput))
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
	// Serialization only runs for a sampled span with capture on.
	capture := p.capture && span != nil && span.IsRecording()
	if capture {
		span.SetAttributes(inputAttrs(memory, p.contentMax)...)
	}
	start := time.Now()

	res, err := p.next.GenerateStream(ctx, memory, availableTools, streamChan)
	if capture && err == nil {
		span.SetAttributes(outputAttr(res, p.contentMax))
	}

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
	name := semconv.OperationChat
	if p.model != "" {
		name += " " + p.model
	}
	return p.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(p.spanAttrs...),
	)
}

func (p *instrumentedProvider) recordDuration(ctx context.Context, d time.Duration) {
	if p.duration == nil {
		return
	}
	p.duration.Record(ctx, d.Seconds(), p.durationOpt)
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
		if res.Usage.PromptTokens > 0 {
			p.tokens.Add(ctx, int64(res.Usage.PromptTokens), p.inputTokOpt)
		}
		if res.Usage.CompletionTokens > 0 {
			p.tokens.Add(ctx, int64(res.Usage.CompletionTokens), p.outputTokOpt)
		}
	}
}
