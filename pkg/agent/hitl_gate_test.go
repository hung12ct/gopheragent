package agent

import (
	"context"
	"testing"
	"time"
)

func TestWithConfirmHITL_RoundTrip(t *testing.T) {
	called := false
	fn := func(_ context.Context, _, _ string) bool {
		called = true
		return true
	}
	ctx := WithConfirmHITL(context.Background(), fn)
	got := ConfirmHITLFromContext(ctx)
	if got == nil {
		t.Fatal("expected ConfirmFunc to be recoverable from ctx, got nil")
	}
	if !got(context.Background(), "any", "{}") || !called {
		t.Fatal("recovered ConfirmFunc did not delegate to the original")
	}
}

func TestWithConfirmHITL_NilIsZeroCost(t *testing.T) {
	// nil fn must return the same ctx; downstream FromContext stays nil.
	parent := context.Background()
	ctx := WithConfirmHITL(parent, nil)
	if ctx != parent {
		t.Fatal("WithConfirmHITL(nil) should not wrap the ctx")
	}
	if ConfirmHITLFromContext(ctx) != nil {
		t.Fatal("expected no ConfirmFunc on a ctx that was never stamped")
	}
}

func TestConfirmHITLFromContext_AbsentReturnsNil(t *testing.T) {
	if ConfirmHITLFromContext(context.Background()) != nil {
		t.Fatal("background ctx should carry no ConfirmFunc")
	}
}

func TestWithConfirmHITLTimeout_RoundTrip(t *testing.T) {
	want := 2 * time.Minute
	ctx := WithConfirmHITLTimeout(context.Background(), want)
	if got := ConfirmHITLTimeoutFromContext(ctx); got != want {
		t.Fatalf("recovered timeout = %s, want %s", got, want)
	}
}

func TestWithConfirmHITLTimeout_ZeroIsZeroCost(t *testing.T) {
	parent := context.Background()
	if ctx := WithConfirmHITLTimeout(parent, 0); ctx != parent {
		t.Fatal("WithConfirmHITLTimeout(0) should not wrap the ctx")
	}
	if ctx := WithConfirmHITLTimeout(parent, -5*time.Second); ctx != parent {
		t.Fatal("WithConfirmHITLTimeout(negative) should not wrap the ctx")
	}
	if got := ConfirmHITLTimeoutFromContext(parent); got != 0 {
		t.Fatalf("expected zero timeout on a never-stamped ctx, got %s", got)
	}
}
