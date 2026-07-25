package telemetry_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/telemetry"
)

// TestNewOTelHandler_ErrorBeforeFirstEvent asserts that an ErrorEvent arriving
// before any Thought/Content/ToolCall event still produces exactly one span with
// error status — the regression fixed in this change (previously zero spans).
func TestNewOTelHandler_ErrorBeforeFirstEvent(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	handler := telemetry.NewOTelHandler(tp.Tracer("test"))

	handler(context.Background(), "s1", agent.Event(agent.ErrorEvent{
		Err:     errors.New("early boom"),
		Message: "early boom",
	}))

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Fatalf("span status = %v, want Error", spans[0].Status().Code)
	}
}
