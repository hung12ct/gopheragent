package tools

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Middleware wraps a Tool to add cross-cutting behaviour (logging, timing, rate limiting…).
// Pattern: each Middleware receives the next Tool in the chain and returns a decorated Tool.
type Middleware func(next Tool) Tool

// Chain applies middlewares to t in declaration order: the first middleware is the outermost
// wrapper (called first on Execute). Equivalent to A(B(C(t))) for Chain(t, A, B, C).
func Chain(t Tool, mws ...Middleware) Tool {
	for i := len(mws) - 1; i >= 0; i-- {
		t = mws[i](t)
	}
	return t
}

// wrappedTool delegates all Tool methods to its inner Tool but overrides Execute.
type wrappedTool struct {
	Tool
	executeFn func(ctx context.Context, argsJSON string) (string, error)
}

func (w *wrappedTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	return w.executeFn(ctx, argsJSON)
}

// WithLogging logs each tool call and result via slog.
//
//	reg.Register(tools.Chain(myTool, tools.WithLogging(slog.Default())))
func WithLogging(logger *slog.Logger) Middleware {
	return func(next Tool) Tool {
		return &wrappedTool{
			Tool: next,
			executeFn: func(ctx context.Context, argsJSON string) (string, error) {
				logger.InfoContext(ctx, "tool call", "tool", next.Name(), "args", argsJSON)
				result, err := next.Execute(ctx, argsJSON)
				if err != nil {
					logger.ErrorContext(ctx, "tool error", "tool", next.Name(), "error", err)
				} else {
					logger.InfoContext(ctx, "tool ok", "tool", next.Name(), "result_bytes", len(result))
				}
				return result, err
			},
		}
	}
}

// WithTiming calls onDone with the tool name, wall-clock duration, and error after each
// Execute call. Zero external dependencies — wire to any metrics system:
//
//	tools.WithTiming(func(name string, d time.Duration, err error) {
//	    prometheus.ObserveToolLatency(name, d.Seconds())
//	})
func WithTiming(onDone func(toolName string, d time.Duration, err error)) Middleware {
	return func(next Tool) Tool {
		return &wrappedTool{
			Tool: next,
			executeFn: func(ctx context.Context, argsJSON string) (string, error) {
				start := time.Now()
				result, err := next.Execute(ctx, argsJSON)
				onDone(next.Name(), time.Since(start), err)
				return result, err
			},
		}
	}
}

// WithTimeout wraps each Execute call with a per-call deadline.
// The parent context's deadline, if shorter, still takes precedence.
func WithTimeout(d time.Duration) Middleware {
	return func(next Tool) Tool {
		return &wrappedTool{
			Tool: next,
			executeFn: func(ctx context.Context, argsJSON string) (string, error) {
				tCtx, cancel := context.WithTimeout(ctx, d)
				defer cancel()
				return next.Execute(tCtx, argsJSON)
			},
		}
	}
}

// WithRateLimit throttles tool execution to at most rps calls per second.
// Excess calls are delayed (not dropped) and respect context cancellation.
// Each application of this middleware creates an independent rate limiter,
// so applying it separately to each tool gives per-tool limits.
func WithRateLimit(rps float64) Middleware {
	if rps <= 0 {
		return func(next Tool) Tool { return next }
	}
	return func(next Tool) Tool {
		interval := time.Duration(float64(time.Second) / rps)
		var mu sync.Mutex
		var lastCall time.Time

		return &wrappedTool{
			Tool: next,
			executeFn: func(ctx context.Context, argsJSON string) (string, error) {
				mu.Lock()
				nextAllowed := lastCall.Add(interval)
				wait := time.Until(nextAllowed)
				mu.Unlock()

				if wait > 0 {
					select {
					case <-time.After(wait):
					case <-ctx.Done():
						return "", fmt.Errorf("rate limit wait cancelled: %w", ctx.Err())
					}
				}

				mu.Lock()
				lastCall = time.Now()
				mu.Unlock()

				return next.Execute(ctx, argsJSON)
			},
		}
	}
}
