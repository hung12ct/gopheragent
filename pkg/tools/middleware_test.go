package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
