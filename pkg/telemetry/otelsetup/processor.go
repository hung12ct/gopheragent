package otelsetup

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/hung12ct/gopheragent/pkg/telemetry/spanattr"
)

// attrProcessor stamps request-scoped attributes onto every span at start:
// the spanattr attributes on the parent ctx, plus allowlisted W3C Baggage
// members. Precedence, lowest to highest: baggage, spanattr, then the
// attributes the instrumentation itself set at Start — an instrumentation's
// own key is never overwritten.
type attrProcessor struct {
	baggageKeys []string
}

var _ sdktrace.SpanProcessor = attrProcessor{}

func (p attrProcessor) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	ctxAttrs := spanattr.From(parent)
	var bag baggage.Baggage
	if len(p.baggageKeys) > 0 {
		bag = baggage.FromContext(parent)
	}
	if len(ctxAttrs) == 0 && bag.Len() == 0 {
		return // hot path: nothing to stamp, no allocation
	}

	existing := s.Attributes()
	add := make([]attribute.KeyValue, 0, len(p.baggageKeys)+len(ctxAttrs))
	for _, k := range p.baggageKeys {
		if m := bag.Member(k); m.Key() != "" && !hasKey(existing, attribute.Key(k)) {
			add = append(add, attribute.String(k, m.Value()))
		}
	}
	for _, kv := range ctxAttrs {
		if !hasKey(existing, kv.Key) {
			add = append(add, kv) // after baggage: the SDK keeps the last duplicate
		}
	}
	if len(add) > 0 {
		s.SetAttributes(add...)
	}
}

func (attrProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (attrProcessor) Shutdown(context.Context) error   { return nil }
func (attrProcessor) ForceFlush(context.Context) error { return nil }

func hasKey(kvs []attribute.KeyValue, k attribute.Key) bool {
	for _, kv := range kvs {
		if kv.Key == k {
			return true
		}
	}
	return false
}
