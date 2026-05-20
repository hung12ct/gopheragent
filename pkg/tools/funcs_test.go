package tools

import (
	"context"
	"strings"
	"testing"
)

type lookupArgs struct {
	UserID string `json:"user_id" description:"the user to look up"`
	Limit  int    `json:"limit,omitempty"`
}

func TestRegisterFunc_HappyPath(t *testing.T) {
	reg := NewRegistry()
	RegisterFunc(reg, "lookup", "Look up a user", func(_ context.Context, a lookupArgs) (Result, error) {
		return Text("found " + a.UserID), nil
	})
	tool, ok := reg.Get("lookup")
	if !ok {
		t.Fatal("tool not registered")
	}
	if tool.Descriptor().Name != "lookup" {
		t.Fatalf("name mismatch: %q", tool.Descriptor().Name)
	}
	if _, hasUserID := tool.Descriptor().Parameters.Properties["user_id"]; !hasUserID {
		t.Fatalf("schema missing user_id: %+v", tool.Descriptor().Parameters)
	}
	res, err := tool.Execute(context.Background(), `{"user_id":"alice"}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Text != "found alice" {
		t.Fatalf("unexpected result: %q", res.Text)
	}
}

func TestRegisterFunc_EmptyArgsIsZeroValue(t *testing.T) {
	reg := NewRegistry()
	RegisterFunc(reg, "noop", "No-args tool", func(_ context.Context, a lookupArgs) (Result, error) {
		if a.UserID != "" || a.Limit != 0 {
			t.Fatalf("expected zero value, got %+v", a)
		}
		return Text("ok"), nil
	})
	tool, _ := reg.Get("noop")
	if _, err := tool.Execute(context.Background(), ""); err != nil {
		t.Fatalf("empty args should be accepted: %v", err)
	}
}

func TestRegisterFunc_BadJSONReturnsToolError(t *testing.T) {
	reg := NewRegistry()
	RegisterFunc(reg, "bad", "Tool with strict args", func(_ context.Context, a lookupArgs) (Result, error) {
		return Text("never"), nil
	})
	tool, _ := reg.Get("bad")
	_, err := tool.Execute(context.Background(), `{not-json`)
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode args") {
		t.Fatalf("expected wrapped error: %v", err)
	}
}

func TestRegisterFunc_OptsAppliedToDescriptor(t *testing.T) {
	reg := NewRegistry()
	RegisterFunc(reg, "hitl", "HITL-gated tool", func(_ context.Context, _ lookupArgs) (Result, error) {
		return Text("ok"), nil
	}, FuncToolOpts{RequiresConfirmation: true, Cacheable: false})
	tool, _ := reg.Get("hitl")
	if !tool.Descriptor().RequiresConfirmation {
		t.Fatal("RequiresConfirmation flag not propagated")
	}
}

func TestRegisterFunc_PropagatesFnError(t *testing.T) {
	reg := NewRegistry()
	RegisterFunc(reg, "fail", "Tool that fails", func(_ context.Context, _ lookupArgs) (Result, error) {
		return Result{}, context.Canceled
	})
	tool, _ := reg.Get("fail")
	_, err := tool.Execute(context.Background(), `{"user_id":"x"}`)
	if err != context.Canceled {
		t.Fatalf("expected ctx.Canceled, got %v", err)
	}
}
