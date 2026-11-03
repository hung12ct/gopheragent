package agent

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
)

// noopEndSpan is the shared end function returned by startIterationSpan when
// telemetry is disabled. Sharing one value avoids allocating a closure per
// iteration on the hot path.
var noopEndSpan = func(error) {}

// noopRunEnd is the shared end function returned by startRunSpan when tracing is
// disabled — one shared value keeps the disabled path allocation-free.
var noopRunEnd = func() {}

// startRunSpan opens the conversation-turn root span and returns the child ctx
// plus its end function. Every iteration (and, through them, the LLM and tool
// spans) nests under it, so a single turn is one trace with one trace ID,
// filterable in the backend by gopheragent.session.key (or the semconv
// gen_ai.conversation.id). When the caller's ctx already carries a span — an
// inbound request, or a parent agent's execute_tool span for a sub-agent — the
// run nests under it instead of starting a new trace. The span is named
// "agent.run" under semconv.SchemaLegacy and "invoke_agent <name>" under
// semconv.SchemaLatest; both carry the GenAI invoke_agent attributes. Returns
// the original ctx and a shared no-op when tracing is disabled (zero cost).
func (al *AgentLoop) startRunSpan(ctx context.Context, sessionKey string) (context.Context, func()) {
	if al.tracer == nil && al.metrics == nil {
		return ctx, noopRunEnd
	}
	rt := &runTelemetry{metrics: al.metrics}
	ctx = context.WithValue(ctx, runTelemetryKey{}, rt)
	var span trace.Span
	if al.tracer != nil {
		attrs := make([]attribute.KeyValue, 0, 4)
		attrs = append(attrs,
			semconv.GenAIOperationName.String(semconv.OperationInvokeAgent),
			semconv.GenAIConversationID.String(sessionKey),
			semconv.SessionKey.String(sessionKey),
		)
		if al.agentName != "" {
			attrs = append(attrs, semconv.GenAIAgentName.String(al.agentName))
		}
		ctx, span = al.tracer.Start(ctx, al.runSpanName(), trace.WithAttributes(attrs...))
	}
	return ctx, func() { al.endRun(ctx, span, rt) }
}

// runSpanName picks the run span name for the configured semconv schema.
func (al *AgentLoop) runSpanName() string {
	if al.semconvSchema != semconv.SchemaLatest {
		return "agent.run"
	}
	if al.agentName == "" {
		return semconv.OperationInvokeAgent
	}
	return semconv.OperationInvokeAgent + " " + al.agentName
}

// WithTracer enables OpenTelemetry tracing of the ReAct loop. Each turn opens a
// run span and each iteration an "agent.iteration" span; because they ride on
// the ctx threaded into the LLM call and tool execution, spans produced by the
// otelllm decorator and the oteltools middleware nest as their children. The
// semconv schema is resolved here, once, from OTEL_SEMCONV_STABILITY_OPT_IN.
// nil (the default) disables tracing at zero hot-path cost — no span is created
// and ctx is untouched.
func WithTracer(t trace.Tracer) Option {
	return func(al *AgentLoop) {
		al.tracer = t
		al.semconvSchema = semconv.SchemaFromEnv()
	}
}

// WithTelemetryFromContext makes a loop built inside a tool — a sub-agent or
// SQL worker — report under its parent. It adopts the TracerProvider of the
// recording span already on ctx (the parent's execute_tool or iteration span),
// so the worker's run span nests as a child in the same trace, and the
// parent's metric instruments, so worker runs and cost are counted (labeled by
// the worker's agent name). No-op for whichever signal the parent has off.
func WithTelemetryFromContext(ctx context.Context) Option {
	return func(al *AgentLoop) {
		if sp := trace.SpanFromContext(ctx); sp.IsRecording() {
			WithTracer(sp.TracerProvider().Tracer(semconv.ScopeName))(al)
		}
		if rt, ok := ctx.Value(runTelemetryKey{}).(*runTelemetry); ok && rt.metrics != nil {
			al.metrics = rt.metrics
		}
	}
}

// WithAgentName sets gen_ai.agent.name on the run span (and, under
// semconv.SchemaLatest, the span name "invoke_agent <name>"). The YAML builder
// sets it from the agent's name, so sub-agents are distinguishable in a trace.
func WithAgentName(name string) Option {
	return func(al *AgentLoop) { al.agentName = name }
}

// WithMeter enables OpenTelemetry metrics for the loop: a per-iteration latency
// histogram (gopheragent.agent.iteration.duration), a run counter by outcome
// (gopheragent.agent.runs), and a run cost counter (gopheragent.agent.cost,
// recorded when a PriceTable or the provider yields a cost). Instruments are
// built once here so nothing allocates per iteration. nil (the default)
// disables metrics.
func WithMeter(m metric.Meter) Option {
	return func(al *AgentLoop) {
		if m == nil {
			al.metrics = nil
			return
		}
		al.metrics = newLoopMetrics(m)
	}
}

// startIterationSpan opens the per-iteration span and returns the child ctx plus
// an end function that closes the span and records the iteration-duration metric.
// When no tracer and no meter is configured it returns the original ctx and a
// no-op end function, performing no allocation — this keeps the loop's hot path
// free when telemetry is off.
func (al *AgentLoop) startIterationSpan(ctx context.Context, sessionKey string, iteration int) (context.Context, func(err error)) {
	if al.tracer == nil && al.metrics == nil {
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
		if al.metrics != nil && al.metrics.iterHist != nil {
			// No session label: one series per conversation would make
			// metric cardinality unbounded. Session lives on the span.
			al.metrics.iterHist.Record(ctx, time.Since(start).Seconds())
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
