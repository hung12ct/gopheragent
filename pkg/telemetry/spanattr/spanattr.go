// Package spanattr carries request-scoped span attributes — tenant, user,
// feature flag, request ID — on a context.Context, so every span started under
// that ctx (agent run, iteration, chat, execute_tool, sub-agent runs) can be
// stamped with them without threading values through each instrumentation
// layer.
//
// Set them once at the request boundary:
//
//	ctx = spanattr.With(ctx,
//	    attribute.String("tenant.id", tenantID),
//	    attribute.String("enduser.id", userID))
//	for ev := range loop.RunText(ctx, sessionKey, input) { ... }
//
// The stamping itself is done by the span processor otelsetup installs; this
// package imports only the OpenTelemetry attribute API and stays free for
// library consumers.
//
// Attributes are in-process only — unlike W3C Baggage they are never written
// to outbound headers, so identifiers cannot leak to downstream services.
// They land on spans, never on metrics: per-tenant or per-user metric labels
// would make series cardinality unbounded.
package spanattr

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
)

type ctxKey struct{}

// With returns a ctx carrying kvs in addition to any attributes already on ctx.
// On a key conflict the later value wins. The parent ctx's attributes are never
// mutated, so concurrent branches can extend the same parent safely.
func With(ctx context.Context, kvs ...attribute.KeyValue) context.Context {
	if len(kvs) == 0 {
		return ctx
	}
	prev := From(ctx)
	merged := make([]attribute.KeyValue, 0, len(prev)+len(kvs))
	merged = append(append(merged, prev...), kvs...)
	return context.WithValue(ctx, ctxKey{}, merged)
}

// From returns the attributes carried on ctx, or nil. Callers must not modify
// the returned slice.
func From(ctx context.Context) []attribute.KeyValue {
	kvs, _ := ctx.Value(ctxKey{}).([]attribute.KeyValue)
	return kvs
}
