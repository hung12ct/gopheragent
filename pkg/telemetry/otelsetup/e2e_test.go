package otelsetup_test

import (
	"context"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/telemetry/otelllm"
	"github.com/hung12ct/gopheragent/pkg/telemetry/otelsetup"
	"github.com/hung12ct/gopheragent/pkg/telemetry/oteltools"
	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
	"github.com/hung12ct/gopheragent/pkg/telemetry/spanattr"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"github.com/hung12ct/gopheragent/pkg/tools/builtin"
)

// Inbound W3C parent the agent run must join.
const (
	inboundTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	inboundSpanID  = "00f067aa0ba902b7"
)

// receiver collects OTLP exports from either transport.
type receiver struct {
	coltracepb.UnimplementedTraceServiceServer
	colmetricpb.UnimplementedMetricsServiceServer

	mu      sync.Mutex
	spans   []*tracepb.Span
	service string
	metrics []*metricpb.Metric
}

func (r *receiver) addTraces(req *coltracepb.ExportTraceServiceRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		for _, a := range rs.GetResource().GetAttributes() {
			if a.GetKey() == "service.name" {
				r.service = a.GetValue().GetStringValue()
			}
		}
		for _, ss := range rs.GetScopeSpans() {
			r.spans = append(r.spans, ss.GetSpans()...)
		}
	}
}

func (r *receiver) addMetrics(req *colmetricpb.ExportMetricsServiceRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			r.metrics = append(r.metrics, sm.GetMetrics()...)
		}
	}
}

func (r *receiver) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	r.addTraces(req)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// metricsServer adapts receiver to the metrics service (Export name clash).
type metricsServer struct {
	colmetricpb.UnimplementedMetricsServiceServer
	r *receiver
}

func (m metricsServer) Export(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	m.r.addMetrics(req)
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

func startGRPC(t *testing.T, r *receiver) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(srv, r)
	colmetricpb.RegisterMetricsServiceServer(srv, metricsServer{r: r})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func startHTTP(t *testing.T, r *receiver) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var resp proto.Message
		switch req.URL.Path {
		case "/v1/traces":
			var in coltracepb.ExportTraceServiceRequest
			if err := proto.Unmarshal(body, &in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.addTraces(&in)
			resp = &coltracepb.ExportTraceServiceResponse{}
		case "/v1/metrics":
			var in colmetricpb.ExportMetricsServiceRequest
			if err := proto.Unmarshal(body, &in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.addMetrics(&in)
			resp = &colmetricpb.ExportMetricsServiceResponse{}
		default:
			http.NotFound(w, req)
			return
		}
		out, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// scriptedLLM routes on the latest message: the parent task delegates to a
// sub-agent, the worker task calls echo, and any tool result ends the turn.
type scriptedLLM struct{}

func (scriptedLLM) GenerateStream(_ context.Context, memory []history.Message, _ *tools.Registry, _ chan<- agent.StreamEvent) (agent.LLMResult, error) {
	last := memory[len(memory)-1]
	usage := agent.TokenUsage{PromptTokens: 10, CompletionTokens: 5}
	switch {
	case last.Role == "tool":
		return agent.LLMResult{Content: "done", Usage: usage}, nil
	case strings.Contains(last.Content, "parent-task"):
		return agent.LLMResult{Usage: usage, ToolCalls: []agent.PendingToolCall{{
			ID: "c1", Name: "call_sub_agent",
			ArgsJSON: `{"task_description":"worker-task","agent_name":"worker"}`,
		}}}, nil
	default:
		return agent.LLMResult{Usage: usage, ToolCalls: []agent.PendingToolCall{{
			ID: "c2", Name: "echo", ArgsJSON: `{"text":"hi"}`,
		}}}, nil
	}
}

type echoArgs struct {
	Text string `json:"text"`
}

// runPipeline wires Setup + every instrumentation layer exactly as an adopter
// would, runs one parent turn that delegates to a sub-agent, and flushes.
func runPipeline(t *testing.T, cfg otelsetup.Config) {
	t.Helper()
	tel, shutdown, err := otelsetup.Setup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !tel.Enabled {
		t.Fatal("Setup reported disabled with an endpoint configured")
	}
	mw := oteltools.Instrument(oteltools.WithTracer(tel.Tracer), oteltools.WithMeter(tel.Meter))
	llm := otelllm.NewProvider(scriptedLLM{}, otelllm.WithSystem("fake"), otelllm.WithModel("m"),
		otelllm.WithTracer(tel.Tracer), otelllm.WithMeter(tel.Meter))

	workerReg := tools.NewRegistry()
	tools.RegisterFunc(workerReg, "echo", "echo text", func(_ context.Context, a echoArgs) (tools.Result, error) {
		return tools.Text(a.Text), nil
	})
	echo, _ := workerReg.Get("echo")
	workerReg.Register(tools.Chain(echo, mw))

	sm := history.NewInMemSessionManager("sys")
	parentReg := tools.NewRegistry()
	parentReg.Register(tools.Chain(builtin.NewCallSubAgentTool(sm, workerReg, llm), mw))
	loop := agent.New(sm, parentReg, llm,
		agent.WithTracer(tel.Tracer), agent.WithMeter(tel.Meter), agent.WithAgentName("planner"))

	carrier := propagation.HeaderCarrier(http.Header{})
	carrier.Set("traceparent", "00-"+inboundTraceID+"-"+inboundSpanID+"-01")
	carrier.Set("baggage", "team.id=core,secret=leak")
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	ctx = spanattr.With(ctx, attribute.String("tenant.id", "acme"))
	if _, err := loop.RunIteration(ctx, "sess-1", "parent-task"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}

	flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown/flush: %v", err)
	}
}

func spanByName(t *testing.T, spans []*tracepb.Span, name, agentName string) *tracepb.Span {
	t.Helper()
	for _, s := range spans {
		if s.GetName() != name {
			continue
		}
		if agentName == "" || strAttr(s.GetAttributes(), string(semconv.GenAIAgentName)) == agentName {
			return s
		}
	}
	t.Fatalf("span %q (agent=%q) not exported; got %v", name, agentName, spanNames(spans))
	return nil
}

func strAttr(attrs []*commonpb.KeyValue, key string) string {
	for _, a := range attrs {
		if a.GetKey() == key {
			return a.GetValue().GetStringValue()
		}
	}
	return ""
}

func hasAttr(attrs []*commonpb.KeyValue, key string) bool {
	for _, a := range attrs {
		if a.GetKey() == key {
			return true
		}
	}
	return false
}

func spanNames(spans []*tracepb.Span) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.GetName())
	}
	return out
}

func assertParent(t *testing.T, child, parent *tracepb.Span) {
	t.Helper()
	if string(child.GetParentSpanId()) != string(parent.GetSpanId()) {
		t.Fatalf("%q parent = %x, want %q (%x)", child.GetName(), child.GetParentSpanId(), parent.GetName(), parent.GetSpanId())
	}
}

// assertTree checks the full exported span tree for one schema.
func assertTree(t *testing.T, r *receiver, runName func(agent string) string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.service != "e2e" {
		t.Fatalf("service.name = %q, want e2e", r.service)
	}
	for _, s := range r.spans {
		if hex.EncodeToString(s.GetTraceId()) != inboundTraceID {
			t.Fatalf("span %q trace = %x — did not join the inbound traceparent", s.GetName(), s.GetTraceId())
		}
		// Request-scoped attributes reach every span, sub-agent spans included.
		if strAttr(s.GetAttributes(), "tenant.id") != "acme" || strAttr(s.GetAttributes(), "team.id") != "core" {
			t.Fatalf("span %q missing request-scoped attrs: %v", s.GetName(), s.GetAttributes())
		}
		if hasAttr(s.GetAttributes(), "secret") {
			t.Fatalf("span %q copied non-allowlisted baggage", s.GetName())
		}
	}
	parentRun := spanByName(t, r.spans, runName("planner"), "planner")
	if hex.EncodeToString(parentRun.GetParentSpanId()) != inboundSpanID {
		t.Fatalf("parent run span parent = %x, want inbound %s", parentRun.GetParentSpanId(), inboundSpanID)
	}
	if strAttr(parentRun.GetAttributes(), string(semconv.GenAIConversationID)) != "sess-1" {
		t.Fatalf("parent run missing gen_ai.conversation.id")
	}
	subTool := spanByName(t, r.spans, "execute_tool call_sub_agent", "")
	workerRun := spanByName(t, r.spans, runName("sub_agent"), "sub_agent")
	assertParent(t, workerRun, subTool)

	echo := spanByName(t, r.spans, "execute_tool echo", "")
	byID := make(map[string]*tracepb.Span, len(r.spans))
	for _, s := range r.spans {
		byID[string(s.GetSpanId())] = s
	}
	for cur := echo; ; {
		p, ok := byID[string(cur.GetParentSpanId())]
		if !ok {
			t.Fatalf("execute_tool echo is not nested under the worker run span")
		}
		if p == workerRun {
			break
		}
		cur = p
	}
}

func assertMetrics(t *testing.T, r *receiver) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]*metricpb.Metric{}
	for _, m := range r.metrics {
		seen[m.GetName()] = m
	}
	for _, want := range []string{
		semconv.MetricLLMDuration, semconv.MetricLLMTokenUsage,
		semconv.MetricToolDuration, semconv.MetricIterationDuration,
	} {
		if seen[want] == nil {
			t.Fatalf("metric %q not exported; got %v", want, keys(seen))
		}
	}
	for _, dp := range seen[semconv.MetricIterationDuration].GetHistogram().GetDataPoints() {
		if hasAttr(dp.GetAttributes(), string(semconv.SessionKey)) {
			t.Fatalf("iteration histogram carries session label (unbounded cardinality)")
		}
	}
	runs := map[string]string{}
	for _, dp := range seen[semconv.MetricAgentRuns].GetSum().GetDataPoints() {
		runs[strAttr(dp.GetAttributes(), string(semconv.GenAIAgentName))] = strAttr(dp.GetAttributes(), string(semconv.RunOutcome))
	}
	if runs["planner"] != semconv.RunOutcomeDone || runs["sub_agent"] != semconv.RunOutcomeDone {
		t.Fatalf("runs counter = %v, want planner and sub_agent both done (worker must inherit the meter)", runs)
	}
	if _, ok := runs["worker"]; ok {
		t.Fatal("model-chosen sub-agent persona leaked into a metric label (unbounded cardinality)")
	}
	for _, dp := range seen[semconv.MetricLLMTokenUsage].GetSum().GetDataPoints() {
		if hasAttr(dp.GetAttributes(), "tenant.id") {
			t.Fatalf("request-scoped attrs leaked onto metrics (unbounded cardinality)")
		}
	}
}

func keys(m map[string]*metricpb.Metric) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func clearOTelEnv(t *testing.T) {
	for _, k := range []string{
		"OTEL_SDK_DISABLED", "OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		semconv.EnvStabilityOptIn, semconv.EnvCaptureContent,
	} {
		t.Setenv(k, "")
	}
}

// TestE2E_ExportsFullTreeOverRealOTLP drives Setup's real exporters against
// in-process OTLP receivers on both transports and asserts the exported span
// tree, inbound trace propagation, sub-agent nesting, and metric shape.
func TestE2E_ExportsFullTreeOverRealOTLP(t *testing.T) {
	legacy := func(string) string { return "agent.run" }
	latest := func(a string) string { return "invoke_agent " + a }
	for _, tc := range []struct {
		name     string
		protocol string
		optIn    string
		runName  func(string) string
	}{
		{"grpc_legacy", otelsetup.ProtocolGRPC, "", legacy},
		{"http_legacy", otelsetup.ProtocolHTTP, "", legacy},
		{"grpc_latest", otelsetup.ProtocolGRPC, semconv.OptInGenAILatest, latest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearOTelEnv(t)
			t.Setenv(semconv.EnvStabilityOptIn, tc.optIn)
			if tc.optIn != "" {
				t.Setenv(semconv.EnvCaptureContent, "true") // latest variant also exercises content capture
			}
			prevTP, prevMP, prevProp := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
			t.Cleanup(func() {
				otel.SetTracerProvider(prevTP)
				otel.SetMeterProvider(prevMP)
				otel.SetTextMapPropagator(prevProp)
			})

			r := &receiver{}
			addr := startGRPC(t, r)
			if tc.protocol == otelsetup.ProtocolHTTP {
				addr = startHTTP(t, r)
			}
			runPipeline(t, otelsetup.Config{
				ServiceName: "e2e", Endpoint: addr, Insecure: true, Protocol: tc.protocol,
				BaggageKeys: []string{"team.id"},
			})
			assertTree(t, r, tc.runName)
			assertMetrics(t, r)

			if tc.optIn != "" {
				chat := spanByName(t, r.spans, "chat m", "")
				if hasAttr(chat.GetAttributes(), string(semconv.GenAISystem)) {
					t.Fatalf("latest schema chat span still carries gen_ai.system")
				}
				if !strings.Contains(strAttr(chat.GetAttributes(), string(semconv.GenAIInputMessages)), "task") {
					t.Fatalf("content capture: chat span lacks gen_ai.input.messages")
				}
				echo := spanByName(t, r.spans, "execute_tool echo", "")
				if strAttr(echo.GetAttributes(), string(semconv.GenAIToolCallResult)) != "hi" {
					t.Fatalf("content capture: echo span lacks gen_ai.tool.call.result")
				}
			} else if hasAttr(spanByName(t, r.spans, "chat m", "").GetAttributes(), string(semconv.GenAIInputMessages)) {
				t.Fatalf("content captured without opt-in")
			}
		})
	}
}
