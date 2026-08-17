package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTimedOut_OwnDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), time.Millisecond, DeadlineCause("tool call", time.Millisecond))
	defer cancel()
	<-ctx.Done()

	if !TimedOut(ctx) {
		t.Fatalf("TimedOut = false, want true (cause = %v)", context.Cause(ctx))
	}
	if !errors.Is(context.Cause(ctx), ErrTimeout) {
		t.Fatal("cause does not match ErrTimeout")
	}
}

// The distinction the sentinel exists for: an enclosing deadline expiring
// first must not read as this layer's own timeout, even though ctx.Err() is
// context.DeadlineExceeded either way.
func TestTimedOut_OuterDeadlineIsNotOurs(t *testing.T) {
	outer, cancelOuter := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancelOuter()
	inner, cancelInner := context.WithTimeoutCause(outer, time.Hour, DeadlineCause("tool call", time.Hour))
	defer cancelInner()
	<-inner.Done()

	if !errors.Is(inner.Err(), context.DeadlineExceeded) {
		t.Fatalf("inner.Err() = %v, want DeadlineExceeded (precondition)", inner.Err())
	}
	if TimedOut(inner) {
		t.Fatal("TimedOut = true for an outer deadline; the layers are indistinguishable")
	}
}

func TestTimedOut_CancellationIsNotATimeout(t *testing.T) {
	outer, cancelOuter := context.WithCancel(context.Background())
	inner, cancelInner := context.WithTimeoutCause(outer, time.Hour, DeadlineCause("tool call", time.Hour))
	defer cancelInner()
	cancelOuter()
	<-inner.Done()

	if TimedOut(inner) {
		t.Fatal("TimedOut = true for a cancelled parent, want false")
	}
}

func TestTimedOut_LiveContext(t *testing.T) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), time.Hour, DeadlineCause("tool call", time.Hour))
	defer cancel()

	if TimedOut(ctx) {
		t.Fatal("TimedOut = true for a context that has not expired")
	}
}

func TestDeadlineCause_NamesTheBudget(t *testing.T) {
	err := DeadlineCause("sql query", 250*time.Millisecond)

	if !errors.Is(err, ErrTimeout) {
		t.Fatal("DeadlineCause does not match ErrTimeout")
	}
	for _, want := range []string{"sql query", "250ms"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("cause %q does not mention %q", err, want)
		}
	}
}

// slowTool blocks until its context ends, then reports why.
type slowTool struct{}

func (slowTool) Descriptor() ToolDescriptor {
	return ToolDescriptor{Name: "slow", Description: "blocks", Display: DefaultDisplay("slow", "blocks")}
}

func (slowTool) Execute(ctx context.Context, _ string) (Result, error) {
	<-ctx.Done()
	return Result{}, ctx.Err()
}

func TestWithTimeout_AttributesOwnDeadline(t *testing.T) {
	tool := Chain(slowTool{}, WithTimeout(10*time.Millisecond))

	_, err := tool.Execute(context.Background(), "{}")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want it to match ErrTimeout", err)
	}
	// Existing matches must keep working: the tool's own error is preserved.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to still match context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "10ms") {
		t.Fatalf("err %q does not name the elapsed budget", err)
	}
}

// A turn cancelled by the caller must not be reported as the tool having
// exceeded its own budget.
func TestWithTimeout_LeavesOuterCancellationUnattributed(t *testing.T) {
	tool := Chain(slowTool{}, WithTimeout(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	_, err := tool.Execute(ctx, "{}")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want no ErrTimeout attribution for a cancelled parent", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// fastTool returns immediately, so the timeout path is never taken.
type fastTool struct{}

func (fastTool) Descriptor() ToolDescriptor {
	return ToolDescriptor{Name: "fast", Description: "returns", Display: DefaultDisplay("fast", "returns")}
}

func (fastTool) Execute(context.Context, string) (Result, error) { return Text("ok"), nil }

func TestWithTimeout_SuccessIsUntouched(t *testing.T) {
	tool := Chain(fastTool{}, WithTimeout(time.Hour))

	res, err := tool.Execute(context.Background(), `{"a":1}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("Text = %q, want %q", res.Text, "ok")
	}
}
