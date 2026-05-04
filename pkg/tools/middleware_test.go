package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockTool is a simple Tool for testing middleware.
type mockTool struct {
	name    string
	execFn  func(ctx context.Context, args string) (string, error)
	latency time.Duration
}

func (m *mockTool) Name() string                    { return m.name }
func (m *mockTool) Description() string             { return "mock" }
func (m *mockTool) ParametersSchema() ToolSchema    { return ToolSchema{} }
func (m *mockTool) RequiresConfirmation() bool       { return false }
func (m *mockTool) Display() ToolDisplay { return DefaultDisplay(m.Name(), m.Description()) }
func (m *mockTool) Execute(ctx context.Context, args string) (string, error) {
	if m.latency > 0 {
		select {
		case <-time.After(m.latency):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if m.execFn != nil {
		return m.execFn(ctx, args)
	}
	return "ok:" + args, nil
}

func TestWithTiming_CallbackFired(t *testing.T) {
	var gotName string
	var gotDuration time.Duration
	var gotErr error
	called := false

	tool := Chain(&mockTool{name: "mytool"}, WithTiming(func(name string, d time.Duration, err error) {
		called = true
		gotName = name
		gotDuration = d
		gotErr = err
	}))

	result, err := tool.Execute(context.Background(), "arg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok:arg" {
		t.Fatalf("unexpected result: %q", result)
	}
	if !called {
		t.Fatal("timing callback not called")
	}
	if gotName != "mytool" {
		t.Fatalf("expected name 'mytool', got %q", gotName)
	}
	if gotDuration < 0 {
		t.Fatal("duration should be non-negative")
	}
	if gotErr != nil {
		t.Fatalf("expected nil error in callback, got: %v", gotErr)
	}
}

func TestWithTiming_ErrorPropagated(t *testing.T) {
	boom := errors.New("tool failed")
	var callbackErr error

	tool := Chain(
		&mockTool{name: "t", execFn: func(_ context.Context, _ string) (string, error) { return "", boom }},
		WithTiming(func(_ string, _ time.Duration, err error) { callbackErr = err }),
	)

	_, err := tool.Execute(context.Background(), "{}")
	if !errors.Is(err, boom) {
		t.Fatalf("expected original error, got: %v", err)
	}
	if !errors.Is(callbackErr, boom) {
		t.Fatalf("expected callback to receive same error, got: %v", callbackErr)
	}
}

func TestWithTimeout_KillsSlowTool(t *testing.T) {
	slow := &mockTool{name: "slow", latency: 200 * time.Millisecond}
	tool := Chain(slow, WithTimeout(20*time.Millisecond))

	_, err := tool.Execute(context.Background(), "{}")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
}

func TestWithTimeout_FastToolSucceeds(t *testing.T) {
	fast := &mockTool{name: "fast"}
	tool := Chain(fast, WithTimeout(500*time.Millisecond))

	result, err := tool.Execute(context.Background(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok:hello" {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestWithRateLimit_ThrottlesExcessCalls(t *testing.T) {
	var execCount int32
	tool := Chain(
		&mockTool{name: "rl", execFn: func(_ context.Context, _ string) (string, error) {
			atomic.AddInt32(&execCount, 1)
			return "ok", nil
		}},
		WithRateLimit(10), // 10 rps → 100ms between calls
	)

	start := time.Now()
	for i := 0; i < 3; i++ {
		_, err := tool.Execute(context.Background(), "{}")
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	// 3 calls at 10 rps = at least 200ms between first and last
	if elapsed < 150*time.Millisecond {
		t.Fatalf("rate limit not enforced: 3 calls finished in %s (expected ≥150ms)", elapsed)
	}
	if atomic.LoadInt32(&execCount) != 3 {
		t.Fatalf("expected 3 executions, got %d", execCount)
	}
}

func TestWithRateLimit_CancelDuringWait(t *testing.T) {
	tool := Chain(&mockTool{name: "rl"}, WithRateLimit(1)) // 1 rps → 1s wait
	// First call to set lastCall
	tool.Execute(context.Background(), "{}") //nolint
	// Second call with immediately-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tool.Execute(ctx, "{}")
	if err == nil {
		t.Fatal("expected error when context cancelled during rate limit wait")
	}
}

func TestChain_OrderPreserved(t *testing.T) {
	var order []string
	makeMiddleware := func(name string) Middleware {
		return func(next Tool) Tool {
			return &wrappedTool{
				Tool: next,
				executeFn: func(ctx context.Context, args string) (string, error) {
					order = append(order, name+":before")
					res, err := next.Execute(ctx, args)
					order = append(order, name+":after")
					return res, err
				},
			}
		}
	}

	tool := Chain(&mockTool{name: "base"}, makeMiddleware("A"), makeMiddleware("B"), makeMiddleware("C"))
	tool.Execute(context.Background(), "{}")

	want := []string{"A:before", "B:before", "C:before", "C:after", "B:after", "A:after"}
	if len(order) != len(want) {
		t.Fatalf("expected %v, got %v", want, order)
	}
	for i, w := range want {
		if order[i] != w {
			t.Fatalf("position %d: expected %q got %q (full: %v)", i, w, order[i], order)
		}
	}
}

func TestWithLogging_DoesNotPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tool := Chain(&mockTool{name: "log_tool"}, WithLogging(logger))
	// Just verify no panic and passthrough works
	result, err := tool.Execute(context.Background(), "payload")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok:payload" {
		t.Fatalf("unexpected result: %q", result)
	}
}

// TestWithLogging_IncludesToolCallID pins the v0.19.0 contract: WithLogging
// auto-reads the per-Execute correlation ID off ctx and emits it on entry
// + exit log records, so adopters get reliable pairing without writing
// custom slog handlers.
func TestWithLogging_IncludesToolCallID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	tool := Chain(&mockTool{name: "log_tool"}, WithLogging(logger))

	ctx := WithToolCallID(context.Background(), "trace-xyz")
	if _, err := tool.Execute(ctx, "payload"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, `"tool_call_id":"trace-xyz"`) {
		t.Fatalf("expected tool_call_id in log output: %s", out)
	}
	// duration_ms should land on the exit lines so latency audits work.
	if !strings.Contains(out, `"duration_ms"`) {
		t.Fatalf("expected duration_ms on exit line: %s", out)
	}
}

// TestWithLogging_ContextExtractor pins the v0.19.0 LoggingOption: adopters
// surface ctx-scoped values (trace_id, user_id, …) without having to write
// a custom slog.Handler bridge.
func TestWithLogging_ContextExtractor(t *testing.T) {
	type traceKey struct{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	tool := Chain(&mockTool{name: "log_tool"}, WithLogging(logger,
		WithContextExtractor(func(ctx context.Context) []slog.Attr {
			if v, ok := ctx.Value(traceKey{}).(string); ok {
				return []slog.Attr{slog.String("trace_id", v)}
			}
			return nil
		}),
	))

	ctx := context.WithValue(context.Background(), traceKey{}, "abc-123")
	if _, err := tool.Execute(ctx, "payload"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), `"trace_id":"abc-123"`) {
		t.Fatalf("extractor did not surface ctx value: %s", buf.String())
	}
}

// TestWithLogging_SuccessLevelDemotesEntryAndOk pins the noise-control knob:
// passing WithSuccessLevel(LevelDebug) sends entry + ok lines below a
// LevelInfo handler's threshold so they get filtered out, while errors
// (always LevelError) still surface.
func TestWithLogging_SuccessLevelDemotesEntryAndOk(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tool := Chain(&mockTool{name: "log_tool"}, WithLogging(logger, WithSuccessLevel(slog.LevelDebug)))

	if _, err := tool.Execute(context.Background(), "payload"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), `"msg":"tool call"`) || strings.Contains(buf.String(), `"msg":"tool ok"`) {
		t.Fatalf("entry/ok lines should be below LevelInfo when demoted to Debug — got: %s", buf.String())
	}

	// Error path still surfaces at LevelError.
	buf.Reset()
	failTool := Chain(&mockTool{
		name:   "fail",
		execFn: func(_ context.Context, _ string) (string, error) { return "", errors.New("boom") },
	}, WithLogging(logger, WithSuccessLevel(slog.LevelDebug)))
	_, _ = failTool.Execute(context.Background(), "payload")
	if !strings.Contains(buf.String(), `"msg":"tool error"`) {
		t.Fatalf("error line must surface even with SuccessLevel=Debug: %s", buf.String())
	}
}

// TestWithLogging_ArgsTruncation pins the args-budget knob: oversized args
// are replaced with their prefix + a length marker so log lines stay
// bounded for tools that accept large blobs.
func TestWithLogging_ArgsTruncation(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	tool := Chain(&mockTool{name: "log_tool"}, WithLogging(logger, WithArgsTruncation(8)))

	bigArgs := strings.Repeat("x", 200)
	if _, err := tool.Execute(context.Background(), bigArgs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "truncated, 200 bytes total") {
		t.Fatalf("expected truncation marker with original length: %s", out)
	}
	if strings.Count(out, "x") > 50 { // 8 prefix bytes + JSON encoding noise; never the full 200
		t.Fatalf("truncation did not cap the args payload: %s", out)
	}
}

func TestMiddleware_PassthroughMethods(t *testing.T) {
	base := &mockTool{name: "base"}
	wrapped := Chain(base, WithTimeout(time.Second))

	if wrapped.Name() != "base" {
		t.Fatalf("Name() should pass through, got %q", wrapped.Name())
	}
	if wrapped.Description() != "mock" {
		t.Fatalf("Description() should pass through")
	}
	if wrapped.RequiresConfirmation() != false {
		t.Fatal("RequiresConfirmation() should pass through")
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

func BenchmarkChain_NoMiddleware(b *testing.B) {
	tool := &mockTool{name: "bench"}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tool.Execute(ctx, "{}")
	}
}

func BenchmarkChain_WithTiming(b *testing.B) {
	tool := Chain(&mockTool{name: "bench"}, WithTiming(func(_ string, _ time.Duration, _ error) {}))
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tool.Execute(ctx, "{}")
	}
}

// schemaTool is a mockTool variant that returns a configurable schema and
// counts ParametersSchema() calls so tests can assert the schema is cached.
type schemaTool struct {
	mockTool
	schema      ToolSchema
	schemaCalls atomic.Int32
}

func (s *schemaTool) ParametersSchema() ToolSchema {
	s.schemaCalls.Add(1)
	return s.schema
}

func schemaObj(required []string, props map[string]any) ToolSchema {
	return ToolSchema{Type: "object", Properties: props, Required: required}
}

func TestWithSchemaValidation_Valid(t *testing.T) {
	st := &schemaTool{
		mockTool: mockTool{name: "search"},
		schema:   schemaObj([]string{"q"}, map[string]any{"q": map[string]any{"type": "string"}}),
	}
	tool := Chain(st, WithSchemaValidation())
	out, err := tool.Execute(context.Background(), `{"q":"hi"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != `ok:{"q":"hi"}` {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestWithSchemaValidation_MissingRequired(t *testing.T) {
	st := &schemaTool{
		mockTool: mockTool{name: "search"},
		schema:   schemaObj([]string{"q"}, map[string]any{"q": map[string]any{"type": "string"}}),
	}
	tool := Chain(st, WithSchemaValidation())
	_, err := tool.Execute(context.Background(), `{}`)
	if err == nil || !containsStr(err.Error(), `missing required property "q"`) {
		t.Fatalf("expected missing-required error, got: %v", err)
	}
}

func TestWithSchemaValidation_NonObject(t *testing.T) {
	st := &schemaTool{
		mockTool: mockTool{name: "search"},
		schema:   schemaObj(nil, nil),
	}
	tool := Chain(st, WithSchemaValidation())
	_, err := tool.Execute(context.Background(), `"just a string"`)
	if err == nil || !containsStr(err.Error(), "expected JSON object") {
		t.Fatalf("expected non-object error, got: %v", err)
	}
}

func TestWithSchemaValidation_MalformedJSON(t *testing.T) {
	st := &schemaTool{
		mockTool: mockTool{name: "search"},
		schema:   schemaObj(nil, nil),
	}
	tool := Chain(st, WithSchemaValidation())
	_, err := tool.Execute(context.Background(), `{not json}`)
	if err == nil || !containsStr(err.Error(), "malformed JSON") {
		t.Fatalf("expected malformed-JSON error, got: %v", err)
	}
}

func TestWithSchemaValidation_TypeMismatch(t *testing.T) {
	st := &schemaTool{
		mockTool: mockTool{name: "search"},
		schema:   schemaObj(nil, map[string]any{"q": map[string]any{"type": "string"}}),
	}
	tool := Chain(st, WithSchemaValidation())
	_, err := tool.Execute(context.Background(), `{"q":123}`)
	if err == nil || !containsStr(err.Error(), "expected string") {
		t.Fatalf("expected type-mismatch error, got: %v", err)
	}
}

func TestWithSchemaValidation_UnknownKeyAllowed(t *testing.T) {
	st := &schemaTool{
		mockTool: mockTool{name: "search"},
		schema:   schemaObj([]string{"q"}, map[string]any{"q": map[string]any{"type": "string"}}),
	}
	tool := Chain(st, WithSchemaValidation())
	_, err := tool.Execute(context.Background(), `{"q":"hi","extra":1}`)
	if err != nil {
		t.Fatalf("unknown keys should be allowed, got: %v", err)
	}
}

func TestWithSchemaValidation_NoSchemaPassthrough(t *testing.T) {
	st := &schemaTool{mockTool: mockTool{name: "free"}}
	tool := Chain(st, WithSchemaValidation())
	_, err := tool.Execute(context.Background(), `anything at all`)
	if err != nil {
		t.Fatalf("no-schema tools should passthrough, got: %v", err)
	}
}

func TestWithSchemaValidation_CachesSchema(t *testing.T) {
	st := &schemaTool{
		mockTool: mockTool{name: "search"},
		schema:   schemaObj([]string{"q"}, map[string]any{"q": map[string]any{"type": "string"}}),
	}
	tool := Chain(st, WithSchemaValidation())
	for i := 0; i < 5; i++ {
		if _, err := tool.Execute(context.Background(), `{"q":"hi"}`); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	if n := st.schemaCalls.Load(); n != 1 {
		t.Fatalf("expected ParametersSchema called once, got %d", n)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
