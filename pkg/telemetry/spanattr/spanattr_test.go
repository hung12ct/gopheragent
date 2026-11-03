package spanattr_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/hung12ct/gopheragent/pkg/telemetry/spanattr"
)

func TestWith_MergesWithoutMutatingParent(t *testing.T) {
	parent := spanattr.With(context.Background(), attribute.String("tenant.id", "acme"))
	a := spanattr.With(parent, attribute.String("enduser.id", "u1"))
	b := spanattr.With(parent, attribute.String("enduser.id", "u2"))

	if got := spanattr.From(parent); len(got) != 1 {
		t.Fatalf("parent mutated: %v", got)
	}
	if got := spanattr.From(a); len(got) != 2 || got[1].Value.AsString() != "u1" {
		t.Fatalf("branch a = %v", got)
	}
	if got := spanattr.From(b); len(got) != 2 || got[1].Value.AsString() != "u2" {
		t.Fatalf("branch b = %v", got)
	}
}

func TestWith_NoAttrsReturnsSameCtx(t *testing.T) {
	ctx := context.Background()
	if spanattr.With(ctx) != ctx {
		t.Fatal("With with no attributes should return ctx unchanged")
	}
	if spanattr.From(ctx) != nil {
		t.Fatal("From on a bare ctx should be nil")
	}
}
