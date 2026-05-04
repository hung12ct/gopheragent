package agent

import (
	"context"
	"errors"
	"time"
)

// RetryAttemptHook fires once per retry attempt, right before the
// exponential-backoff wait. attempt is 1-indexed (first retry = 1). err
// is the failure that triggered the retry. nextDelay is how long the
// loop will sleep before the next call. The hook runs synchronously on
// the loop goroutine — keep it cheap (log line, metric increment, span
// event). Adopters use it to answer "is the run slow because of retries
// or one slow call?" without parsing thought events.
type RetryAttemptHook func(ctx context.Context, attempt int, err error, nextDelay time.Duration)

// RetryConfig controls exponential-backoff retry behaviour for LLM calls.
// Only transient errors are retried; context cancellation / deadline errors are not.
// Retry is silently skipped if any "content" event has already been streamed to the
// client — partial responses cannot be rewound.
type RetryConfig struct {
	// MaxRetries is the maximum number of additional attempts after the first failure.
	// 0 disables retries (default).
	MaxRetries int

	// BaseDelay is the initial wait before the first retry (default: 500ms).
	BaseDelay time.Duration

	// MaxDelay caps the exponential growth (default: 30s).
	MaxDelay time.Duration

	// OnAttempt, when non-nil, fires once per retry attempt with the
	// structured (attempt, err, nextDelay) tuple. See RetryAttemptHook.
	OnAttempt RetryAttemptHook
}

// DefaultRetryConfig returns a sensible default: 3 retries, 500ms base, 30s cap.
func DefaultRetryConfig() *RetryConfig {
	return &RetryConfig{
		MaxRetries: 3,
		BaseDelay:  500 * time.Millisecond,
		MaxDelay:   30 * time.Second,
	}
}

// delay returns the backoff duration for the given attempt index (0-based).
func (r *RetryConfig) delay(attempt int) time.Duration {
	if r.BaseDelay <= 0 {
		r.BaseDelay = 500 * time.Millisecond
	}
	d := r.BaseDelay * (1 << uint(attempt)) // 500ms, 1s, 2s, 4s…
	if r.MaxDelay > 0 && d > r.MaxDelay {
		d = r.MaxDelay
	}
	return d
}

// isRetryable returns false for context-level errors that should not be retried.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
