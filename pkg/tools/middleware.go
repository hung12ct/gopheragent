package tools

import (
	"context"
	"encoding/json"
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

// LoggingOption customizes WithLogging. Use the With* helpers below.
type LoggingOption func(*loggingConfig)

type loggingConfig struct {
	extractor       func(context.Context) []slog.Attr
	successLevel    slog.Level
	successLevelSet bool
	argsTruncate    int
}

// WithSuccessLevel sets the slog level for the entry ("tool call") and
// successful exit ("tool ok") log lines. Errors always log at slog.LevelError
// regardless. Default is slog.LevelInfo. Demote to slog.LevelDebug in
// production to keep error visibility while silencing healthy traffic:
//
//	tools.WithLogging(prodLogger, tools.WithSuccessLevel(slog.LevelDebug))
func WithSuccessLevel(level slog.Level) LoggingOption {
	return func(c *loggingConfig) {
		c.successLevel = level
		c.successLevelSet = true
	}
}

// WithArgsTruncation caps the args string included on the entry log line at
// maxBytes; longer args are replaced with their prefix plus a length marker.
// 0 (default) disables truncation. Use to keep log lines bounded when tools
// accept large blobs (image data URIs, multi-KB SQL, etc).
func WithArgsTruncation(maxBytes int) LoggingOption {
	return func(c *loggingConfig) { c.argsTruncate = maxBytes }
}

// WithContextExtractor lets adopters surface ctx-scoped values (trace_id,
// user_id, request tags) on every tool log line without writing a custom
// slog.Handler bridge. The extractor runs on every Execute and its attrs are
// appended to the entry/exit log records. nil extractors are a no-op.
//
//	tools.WithLogging(logger,
//	    tools.WithContextExtractor(func(ctx context.Context) []slog.Attr {
//	        return []slog.Attr{
//	            slog.String("trace_id", trace.SpanContextFromContext(ctx).TraceID().String()),
//	            slog.String("user_id", auth.UserIDFromContext(ctx)),
//	        }
//	    }),
//	)
func WithContextExtractor(fn func(context.Context) []slog.Attr) LoggingOption {
	return func(c *loggingConfig) { c.extractor = fn }
}

// WithLogging logs each tool call and result via slog. The tool-call
// correlation ID set by the agent loop (see ToolCallIDFromContext) is included
// on entry and exit so log scrapers can pair them reliably even when
// SpeculativeTools=true interleaves parallel calls.
//
//	reg.Register(tools.Chain(myTool, tools.WithLogging(slog.Default())))
//
// Pair with WithContextExtractor to surface trace IDs, tenant tags, etc.
func WithLogging(logger *slog.Logger, opts ...LoggingOption) Middleware {
	cfg := &loggingConfig{successLevel: slog.LevelInfo}
	for _, opt := range opts {
		opt(cfg)
	}
	if !cfg.successLevelSet {
		cfg.successLevel = slog.LevelInfo
	}
	return func(next Tool) Tool {
		return &wrappedTool{
			Tool: next,
			executeFn: func(ctx context.Context, argsJSON string) (string, error) {
				attrs := []slog.Attr{
					slog.String("tool", next.Name()),
					slog.String("tool_call_id", ToolCallIDFromContext(ctx)),
				}
				if cfg.extractor != nil {
					attrs = append(attrs, cfg.extractor(ctx)...)
				}
				start := time.Now()
				logger.LogAttrs(ctx, cfg.successLevel, "tool call", append(attrs, slog.String("args", truncateArgs(argsJSON, cfg.argsTruncate)))...)
				result, err := next.Execute(ctx, argsJSON)
				exitAttrs := append(attrs, slog.Int64("duration_ms", time.Since(start).Milliseconds()))
				if err != nil {
					logger.LogAttrs(ctx, slog.LevelError, "tool error", append(exitAttrs, slog.Any("error", err))...)
				} else {
					logger.LogAttrs(ctx, cfg.successLevel, "tool ok", append(exitAttrs, slog.Int("result_bytes", len(result)))...)
				}
				return result, err
			},
		}
	}
}

// truncateArgs returns args unchanged when maxBytes is 0 or the string fits;
// otherwise it returns the prefix plus a length marker so log scrapers can
// see how much was elided without scanning the full payload.
func truncateArgs(args string, maxBytes int) string {
	if maxBytes <= 0 || len(args) <= maxBytes {
		return args
	}
	return args[:maxBytes] + fmt.Sprintf("...(truncated, %d bytes total)", len(args))
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

// WithSchemaValidation validates argsJSON against the tool's ParametersSchema
// before delegating to Execute. It implements a practical subset of JSON
// Schema: JSON well-formedness, object-type check, required-property check,
// and per-property primitive-type check (string / number / integer / boolean
// / array / object). Unknown properties are accepted (draft-07 default).
// No enum, pattern, minimum, additionalProperties, $ref, or nested property
// recursion beyond the top level.
//
// Tools whose ParametersSchema() returns a zero value (no type, no
// properties, no required) are passed through unchanged.
//
// The schema is captured once per wrap so ParametersSchema() is not re-read
// on every call.
func WithSchemaValidation() Middleware {
	return func(next Tool) Tool {
		schema := next.ParametersSchema()
		empty := schema.Type == "" && len(schema.Properties) == 0 && len(schema.Required) == 0
		return &wrappedTool{
			Tool: next,
			executeFn: func(ctx context.Context, argsJSON string) (string, error) {
				if empty {
					return next.Execute(ctx, argsJSON)
				}
				var raw any
				if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
					return "", fmt.Errorf("tools: schema validation failed for %s: malformed JSON: %w", next.Name(), err)
				}
				if schema.Type == "object" {
					obj, ok := raw.(map[string]any)
					if !ok {
						return "", fmt.Errorf("tools: schema validation failed for %s: expected JSON object", next.Name())
					}
					for _, req := range schema.Required {
						if _, present := obj[req]; !present {
							return "", fmt.Errorf("tools: schema validation failed for %s: missing required property %q", next.Name(), req)
						}
					}
					for name, def := range schema.Properties {
						v, present := obj[name]
						if !present {
							continue
						}
						defMap, ok := def.(map[string]any)
						if !ok {
							continue
						}
						declared, ok := defMap["type"].(string)
						if !ok || declared == "" {
							continue
						}
						if !matchJSONType(declared, v) {
							return "", fmt.Errorf("tools: schema validation failed for %s: property %q: expected %s, got %T", next.Name(), name, declared, v)
						}
					}
				}
				return next.Execute(ctx, argsJSON)
			},
		}
	}
}

// matchJSONType reports whether v (as decoded by encoding/json into `any`)
// matches the JSON Schema primitive type name. Numbers decode as float64;
// "integer" accepts any float64 value (whole-number check deferred to the
// tool itself). Unknown declared types return true (permissive).
func matchJSONType(declared string, v any) bool {
	switch declared {
	case "string":
		_, ok := v.(string)
		return ok
	case "number", "integer":
		_, ok := v.(float64)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "null":
		return v == nil
	default:
		return true
	}
}
