package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// flakyProvider fails the first failBefore calls with a transient error,
// then returns a successful response.
type flakyProvider struct {
	mu         sync.Mutex
	failBefore int
	calls      int
	transient  error
	finalText  string
}

func (p *flakyProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n <= p.failBefore {
		return LLMResult{}, p.transient
	}
	ch <- StreamEvent{Type: EventTypeContent, Content: p.finalText}
	return LLMResult{Content: p.finalText}, nil
}

func TestRetry_OnAttemptFiresOncePerAttempt(t *testing.T) {
	transient := errors.New("rate limited")
	prov := &flakyProvider{failBefore: 2, transient: transient, finalText: "ok"}
	loop, _ := setup(prov)
	loop.Retry = &RetryConfig{
		MaxRetries: 5,
		BaseDelay:  1 * time.Millisecond,
		MaxDelay:   1 * time.Millisecond,
	}

	type sample struct {
		attempt   int
		err       error
		nextDelay time.Duration
	}
	var got []sample
	var mu sync.Mutex
	loop.Retry.OnAttempt = func(_ context.Context, attempt int, err error, nextDelay time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, sample{attempt: attempt, err: err, nextDelay: nextDelay})
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// failBefore=2 → first call fails, then 2 retries (one of which succeeds
	// on the 3rd call). The hook fires before each retry — that's 2 fires.
	if len(got) != 2 {
		t.Fatalf("expected 2 attempt hooks, got %d: %+v", len(got), got)
	}
	if got[0].attempt != 1 || got[1].attempt != 2 {
		t.Fatalf("attempt numbers wrong: got %+v, want [1 2]", got)
	}
	for i, s := range got {
		if !errors.Is(s.err, transient) {
			t.Errorf("hook[%d].err: got %v, want transient", i, s.err)
		}
		if s.nextDelay <= 0 {
			t.Errorf("hook[%d].nextDelay should be > 0, got %v", i, s.nextDelay)
		}
	}
}

func TestRetry_OnAttemptNotFiredOnFirstSuccess(t *testing.T) {
	prov := &flakyProvider{failBefore: 0, finalText: "ok"}
	loop, _ := setup(prov)
	loop.Retry = &RetryConfig{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}

	called := 0
	loop.Retry.OnAttempt = func(_ context.Context, _ int, _ error, _ time.Duration) {
		called++
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != 0 {
		t.Fatalf("OnAttempt should not fire when the first call succeeds, fired %d times", called)
	}
}

func TestRetry_OnAttemptNilIsZeroCost(t *testing.T) {
	// Sanity check: leaving OnAttempt nil must not crash the retry loop.
	transient := errors.New("rate limited")
	prov := &flakyProvider{failBefore: 1, transient: transient, finalText: "ok"}
	loop, _ := setup(prov)
	loop.Retry = &RetryConfig{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	// loop.Retry.OnAttempt left nil

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Compile-time check that history is wired through; the import is used by
// the flakyProvider receiver method.
var _ = history.Message{}
