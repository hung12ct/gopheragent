package agent

import (
	"context"
	"errors"
	"sync"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
)

// loopMetrics holds the instruments built once by WithMeter. It is shared by
// pointer, so a worker loop adopting its parent's telemetry
// (WithTelemetryFromContext) records into the same instruments. A field whose
// instrument failed to build stays nil and is skipped.
type loopMetrics struct {
	iterHist metric.Float64Histogram
	runs     metric.Int64Counter
	cost     metric.Float64Counter
}

func newLoopMetrics(m metric.Meter) *loopMetrics {
	lm := &loopMetrics{}
	if h, err := m.Float64Histogram(semconv.MetricIterationDuration,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a single ReAct iteration."),
		metric.WithExplicitBucketBoundaries(semconv.DurationBucketsSeconds...),
	); err == nil {
		lm.iterHist = h
	}
	if c, err := m.Int64Counter(semconv.MetricAgentRuns,
		metric.WithUnit("{run}"),
		metric.WithDescription("Finished agent runs by terminal outcome."),
	); err == nil {
		lm.runs = c
	}
	if c, err := m.Float64Counter(semconv.MetricAgentCost,
		metric.WithUnit("{USD}"),
		metric.WithDescription("Agent run cost in US dollars."),
	); err == nil {
		lm.cost = c
	}
	return lm
}

// runTelemetry is the per-run tracker installed on ctx by startRunSpan. emit
// feeds it every terminal event; endRun reads the result once. The mutex
// covers terminals emitted from parallel tool goroutines.
type runTelemetry struct {
	metrics *loopMetrics

	mu      sync.Mutex
	outcome string
	err     error
}

type runTelemetryKey struct{}

// observeTerminal records a terminal event of this loop's own run. Forwarded
// sub-agent events (Source set) belong to the worker's run, not this one.
func observeTerminal(ctx context.Context, ev StreamEvent) {
	if (ev.Type != EventTypeDone && ev.Type != EventTypeError) || ev.Source != "" {
		return
	}
	rt, ok := ctx.Value(runTelemetryKey{}).(*runTelemetry)
	if !ok {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if ev.Type == EventTypeError {
		if rt.err == nil { // the first error is the cause; later ones are fallout
			rt.err = terminalErr(ev)
			rt.outcome = classifyRunError(rt.err)
		}
		return
	}
	if rt.outcome == "" {
		rt.outcome = semconv.RunOutcomeDone
	}
}

// terminalErr extracts the structured error of an error event, falling back to
// its message when the emitter attached none.
func terminalErr(ev StreamEvent) error {
	p, _ := ev.Payload.(ErrorEvent)
	if p.Err != nil {
		return p.Err
	}
	return errors.New(p.Message)
}

func (rt *runTelemetry) result() (string, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.outcome == "" {
		return semconv.RunOutcomeUnknown, nil
	}
	return rt.outcome, rt.err
}

// classifyRunError maps a terminal error onto the closed RunOutcome set.
func classifyRunError(err error) string {
	switch {
	case errors.Is(err, ErrContextCancelled), errors.Is(err, context.Canceled):
		return semconv.RunOutcomeCancelled
	case errors.Is(err, ErrMaxIterations):
		return semconv.RunOutcomeMaxIterations
	case errors.Is(err, ErrMaxToolCallsPerSession):
		return semconv.RunOutcomeToolCallCap
	case errors.Is(err, ErrLoopDetected):
		return semconv.RunOutcomeLoopDetected
	case errors.Is(err, ErrHookRejected):
		return semconv.RunOutcomeHookRejected
	case errors.Is(err, ErrApprovalPending):
		return semconv.RunOutcomeApprovalPending
	case errors.Is(err, ErrLLMFailure), errors.Is(err, ErrLLMAuth),
		errors.Is(err, ErrLLMTruncated), errors.Is(err, ErrLLMContentBlocked):
		return semconv.RunOutcomeLLMError
	}
	return semconv.RunOutcomeError
}

// endRun closes the run span with its outcome and records the run and cost
// metrics. A cancelled run and a run paused for deferred approval keep an
// unset span status: neither is a failure.
func (al *AgentLoop) endRun(ctx context.Context, span trace.Span, rt *runTelemetry) {
	outcome, err := rt.result()
	if m := al.metrics; m != nil {
		agentAttr := semconv.GenAIAgentName.String(al.agentName)
		if m.runs != nil {
			m.runs.Add(ctx, 1, metric.WithAttributes(semconv.RunOutcome.String(outcome), agentAttr))
		}
		if acc := runCostAccFromContext(ctx); acc != nil && m.cost != nil {
			if _, usd := acc.snapshot(); usd > 0 {
				m.cost.Add(ctx, usd, metric.WithAttributes(agentAttr))
			}
		}
	}
	if span == nil {
		return
	}
	span.SetAttributes(semconv.RunOutcome.String(outcome))
	switch {
	case outcome == semconv.RunOutcomeDone:
		span.SetStatus(codes.Ok, "")
	case err != nil && outcome != semconv.RunOutcomeCancelled && outcome != semconv.RunOutcomeApprovalPending:
		span.RecordError(err)
		span.SetStatus(codes.Error, outcome)
	}
	span.End()
}

// noopHITLEnd is shared so the tracing-off approval path allocates nothing.
var noopHITLEnd = func(string) {}

// startHITLSpan opens a "hitl.await_approval" span around the human-approval
// wait, so time blocked on a person is visible in the trace and kept apart
// from model and tool latency. The returned func records the decision.
func (al *AgentLoop) startHITLSpan(ctx context.Context, tc PendingToolCall) (context.Context, func(decision string)) {
	if al.tracer == nil {
		return ctx, noopHITLEnd
	}
	ctx, span := al.tracer.Start(ctx, "hitl.await_approval", trace.WithAttributes(
		semconv.GenAIToolName.String(tc.Name),
		semconv.GenAIToolCallID.String(tc.ID),
	))
	return ctx, func(decision string) {
		span.SetAttributes(semconv.HITLDecision.String(decision))
		span.End()
	}
}
