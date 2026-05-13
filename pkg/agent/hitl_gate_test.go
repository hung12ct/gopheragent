package agent

import (
	"context"
	"testing"
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
