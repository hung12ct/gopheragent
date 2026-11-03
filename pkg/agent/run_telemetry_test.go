package agent

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// alwaysToolProvider never answers, so the loop exhausts MaxIters.
type alwaysToolProvider struct{ n int }

func (p *alwaysToolProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, _ chan<- StreamEvent) (LLMResult, error) {
	p.n++
	return LLMResult{ToolCalls: []PendingToolCall{{ID: fmt.Sprintf("c%d", p.n), Name: "echo", ArgsJSON: fmt.Sprintf(`{"i":%d}`, p.n)}}}, nil
}

type runRecorders struct {
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
	opts   []Option
}

func newRunRecorders() runRecorders {
	sr := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return runRecorders{spans: sr, reader: reader, opts: []Option{
		WithTracer(tp.Tracer("t")), WithMeter(mp.Meter("t")), WithAgentName("a"),
	}}
}

func (r runRecorders) rootSpan(t *testing.T) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range r.spans.Ended() {
		if !s.Parent().IsValid() {
			return s
		}
	}
	t.Fatal("no run span")
	return nil
}

// runsByOutcome returns gopheragent.agent.runs counts keyed by outcome, plus
// the summed cost.
func (r runRecorders) runsAndCost(t *testing.T) (map[string]int64, float64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	runs, cost := map[string]int64{}, 0.0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				if m.Name != semconv.MetricAgentRuns {
					continue
				}
				for _, dp := range d.DataPoints {
					o, _ := dp.Attributes.Value(semconv.RunOutcome)
					if a, _ := dp.Attributes.Value(semconv.GenAIAgentName); a.AsString() != "a" {
						t.Fatalf("runs counter missing agent name: %v", dp.Attributes.ToSlice())
					}
					runs[o.AsString()] += dp.Value
				}
			case metricdata.Sum[float64]:
				if m.Name == semconv.MetricAgentCost {
					for _, dp := range d.DataPoints {
						cost += dp.Value
					}
				}
			}
		}
	}
	return runs, cost
}

func spanAttr(s sdktrace.ReadOnlySpan, k attribute.Key) string {
	for _, a := range s.Attributes() {
		if a.Key == k {
			return a.Value.AsString()
		}
	}
	return ""
}

func TestRunOutcome_DoneRecordsCostAndOkStatus(t *testing.T) {
	r := newRunRecorders()
	p := &scriptProvider{turns: []LLMResult{{Content: "hi", Usage: TokenUsage{PromptTokens: 1_000_000, TotalTokens: 1_000_000}}}}
	loop := New(history.NewInMemSessionManager("sys"), tools.NewRegistry(), p, r.opts...)
	loop.PriceTable = PriceTable{"m": {InputPerMTokens: 3}}
	loop.PriceModel = "m"
	if _, err := loop.RunIteration(context.Background(), "s", "q"); err != nil {
		t.Fatal(err)
	}
	run := r.rootSpan(t)
	if spanAttr(run, semconv.RunOutcome) != semconv.RunOutcomeDone || run.Status().Code != codes.Ok {
		t.Fatalf("outcome=%q status=%v", spanAttr(run, semconv.RunOutcome), run.Status())
	}
	runs, cost := r.runsAndCost(t)
	if runs[semconv.RunOutcomeDone] != 1 || len(runs) != 1 {
		t.Fatalf("runs = %v, want exactly one done", runs)
	}
	if cost < 2.99 || cost > 3.01 {
		t.Fatalf("cost = %v, want 3 USD for 1M input tokens at $3/M", cost)
	}
}

func TestRunOutcome_MaxIterationsMarksSpanError(t *testing.T) {
	r := newRunRecorders()
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "echo"})
	loop := New(history.NewInMemSessionManager("sys"), reg, &alwaysToolProvider{}, append(r.opts, WithMaxIters(2))...)
	_, _ = loop.RunIteration(context.Background(), "s", "q")

	run := r.rootSpan(t)
	if got := spanAttr(run, semconv.RunOutcome); got != semconv.RunOutcomeMaxIterations {
		t.Fatalf("outcome = %q, want max_iterations", got)
	}
	if run.Status().Code != codes.Error {
		t.Fatalf("status = %v, want Error", run.Status())
	}
	if runs, _ := r.runsAndCost(t); runs[semconv.RunOutcomeMaxIterations] != 1 {
		t.Fatalf("runs = %v", runs)
	}
}

func TestRunOutcome_CancelIsNotAFailure(t *testing.T) {
	r := newRunRecorders()
	loop := New(history.NewInMemSessionManager("sys"), tools.NewRegistry(), cancelProvider{}, r.opts...)
	_, _ = loop.RunIteration(context.Background(), "s", "q")

	run := r.rootSpan(t)
	if got := spanAttr(run, semconv.RunOutcome); got != semconv.RunOutcomeCancelled {
		t.Fatalf("outcome = %q, want cancelled", got)
	}
	if run.Status().Code != codes.Unset {
		t.Fatalf("cancelled run status = %v, want Unset", run.Status())
	}
}

func TestObserveTerminal_IgnoresForwardedEvents(t *testing.T) {
	rt := &runTelemetry{}
	ctx := context.WithValue(context.Background(), runTelemetryKey{}, rt)
	fwd := Event(ErrorEvent{Err: ErrLoopDetected})
	fwd.Source = "subagent:worker"
	observeTerminal(ctx, fwd)
	observeTerminal(ctx, Event(DoneEvent{}))
	if o, _ := rt.result(); o != semconv.RunOutcomeDone {
		t.Fatalf("outcome = %q: a sub-agent's forwarded error must not fail the parent run", o)
	}
}

func TestHITLSpan_RecordsDecision(t *testing.T) {
	for _, tc := range []struct {
		approve bool
		want    string
	}{{true, "approved"}, {false, "denied"}} {
		r := newRunRecorders()
		p := &scriptProvider{turns: []LLMResult{{ToolCalls: []PendingToolCall{{ID: "c1", Name: "danger", ArgsJSON: `{}`}}}}}
		reg := tools.NewRegistry()
		reg.Register(&echoTool{name: "danger", confirm: true})
		loop := New(history.NewInMemSessionManager("sys"), reg, p, r.opts...)
		loop.ConfirmHITL = func(context.Context, string, string) bool { return tc.approve }
		_, _ = loop.RunIteration(context.Background(), "s", "q")

		var found bool
		for _, s := range r.spans.Ended() {
			if s.Name() == "hitl.await_approval" {
				found = true
				if got := spanAttr(s, semconv.HITLDecision); got != tc.want {
					t.Fatalf("decision = %q, want %q", got, tc.want)
				}
			}
		}
		if !found {
			t.Fatalf("no hitl.await_approval span (approve=%v)", tc.approve)
		}
	}
}

func TestRunAndHITLPaths_ZeroAllocWhenDisabled(t *testing.T) {
	al := &AgentLoop{}
	ctx := context.Background()
	tc := PendingToolCall{Name: "x"}
	allocs := testing.AllocsPerRun(1000, func() {
		_, end := al.startRunSpan(ctx, "s")
		end()
		_, endWait := al.startHITLSpan(ctx, tc)
		endWait("approved")
	})
	if allocs != 0 {
		t.Fatalf("disabled run/HITL telemetry allocated %v times", allocs)
	}
}

func TestRunOutcome_ApprovalPendingIsNotAFailure(t *testing.T) {
	r := newRunRecorders()
	p := &scriptProvider{turns: []LLMResult{{ToolCalls: []PendingToolCall{{ID: "c1", Name: "danger", ArgsJSON: `{}`}}}}}
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "danger", confirm: true})
	loop := New(history.NewInMemSessionManager("sys"), reg, p, r.opts...)
	loop.DeferApprovals = true
	_, _ = loop.RunIteration(context.Background(), "s", "q")

	run := r.rootSpan(t)
	if got := spanAttr(run, semconv.RunOutcome); got != semconv.RunOutcomeApprovalPending {
		t.Fatalf("outcome = %q, want approval_pending", got)
	}
	if run.Status().Code != codes.Unset {
		t.Fatalf("paused run status = %v, want Unset (a pause is not a failure)", run.Status())
	}
}

func TestHITLSpan_CancelledMidWait(t *testing.T) {
	r := newRunRecorders()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &scriptProvider{turns: []LLMResult{{ToolCalls: []PendingToolCall{{ID: "c1", Name: "danger", ArgsJSON: `{}`}}}}}
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "danger", confirm: true})
	loop := New(history.NewInMemSessionManager("sys"), reg, p, r.opts...)
	loop.ConfirmHITL = func(context.Context, string, string) bool { cancel(); return false }
	_, _ = loop.RunIteration(ctx, "s", "q")

	for _, s := range r.spans.Ended() {
		if s.Name() == "hitl.await_approval" {
			if got := spanAttr(s, semconv.HITLDecision); got != "cancelled" {
				t.Fatalf("decision = %q, want cancelled (nobody decided)", got)
			}
			return
		}
	}
	t.Fatal("no hitl.await_approval span")
}
