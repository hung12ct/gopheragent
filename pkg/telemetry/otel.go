// Package telemetry provides OpenTelemetry integration for GopherAgent via the EventHandler system.
// It is an optional import: applications that do not need tracing never pay for it.
//
// Wire it at startup:
//
//	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
//	tracer := tp.Tracer("gopheragent")
//	loop.OnEvent(telemetry.NewOTelHandler(tracer))
package telemetry

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/hung12ct/gopheragent/pkg/agent"
)

// iterSpan holds an in-progress iteration span and the context that carries it.
type iterSpan struct {
	span trace.Span
	ctx  context.Context
}

// NewOTelHandler returns an agent.EventHandler that instruments every agent iteration
// with OpenTelemetry tracing.
//
// Span lifecycle per session key:
//   - Root span "agent.iteration" is opened lazily on the first event.
//   - "tool_call" → span event "tool.execute" with tool name attribute.
//   - "action_required" → span event "hitl.action_required".
//   - "error" → RecordError + SetStatus(Error) + End.
//   - "done" → SetStatus(Ok) + End.
//
// The tracer's parent context is the ctx passed to RunIteration / RunIterationStream,
// so the agent spans nest correctly inside existing traces.
func NewOTelHandler(tracer trace.Tracer) agent.EventHandler {
	var spans sync.Map // sessionKey string → *iterSpan

	getOrCreate := func(ctx context.Context, sessionKey string) *iterSpan {
		if val, ok := spans.Load(sessionKey); ok {
			return val.(*iterSpan)
		}
		spanCtx, span := tracer.Start(ctx, "agent.iteration",
			trace.WithAttributes(attribute.String("session.key", sessionKey)),
		)
		s := &iterSpan{span: span, ctx: spanCtx}
		actual, loaded := spans.LoadOrStore(sessionKey, s)
		if loaded {
			// Lost the race — end the duplicate and return the winner.
			span.End()
			return actual.(*iterSpan)
		}
		return s
	}

	return func(ctx context.Context, sessionKey string, ev agent.StreamEvent) {
		switch ev.Type {
		case "thought", "content":
			// Lazy-create the root span on the first event of the iteration.
			getOrCreate(ctx, sessionKey)

		case "tool_call":
			if s := getOrCreate(ctx, sessionKey); s != nil {
				s.span.AddEvent("tool.execute",
					trace.WithAttributes(attribute.String("tool.call", ev.Content)),
				)
			}

		case "action_required":
			if s := getOrCreate(ctx, sessionKey); s != nil {
				s.span.AddEvent("hitl.action_required",
					trace.WithAttributes(attribute.String("payload", ev.Content)),
				)
			}

		case "error":
			if val, ok := spans.LoadAndDelete(sessionKey); ok {
				s := val.(*iterSpan)
				if ev.Err != nil {
					s.span.RecordError(ev.Err)
					s.span.SetStatus(codes.Error, ev.Err.Error())
				} else {
					s.span.SetStatus(codes.Error, ev.Content)
				}
				s.span.End()
			}

		case "done":
			if val, ok := spans.LoadAndDelete(sessionKey); ok {
				s := val.(*iterSpan)
				s.span.SetStatus(codes.Ok, "")
				s.span.End()
			}
		}
	}
}
