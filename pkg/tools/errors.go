package tools

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Error classification sentinels. Tool authors wrap their real error with
// one of these to let the agent loop and clients distinguish the failure
// category:
//
//   - ErrUser       — the caller supplied bad input; show the message to
//     the user, do not retry.
//   - ErrTransient  — transient downstream failure (rate limit, 5xx,
//     network timeout); retrying later may succeed.
//   - ErrPermanent  — non-recoverable downstream failure (bad config,
//     permanent 4xx from an external API); retries will
//     keep failing.
//
// Wrap pattern:
//
//	return tools.Result{}, fmt.Errorf("rate limited: %w", tools.ErrTransient)
//
// The loop surfaces the classification on the error stream event so clients
// can render differently (toast for ErrUser, retry banner for ErrTransient,
// hard fail for ErrPermanent). errors.Is against the sentinels is the match
// mechanism; use ClassifyError to get the sentinel directly from an
// arbitrary error.
var (
	// ErrUser flags errors caused by bad user/caller input.
	ErrUser = errors.New("tools: user error")

	// ErrTransient flags transient failures that may succeed on retry.
	ErrTransient = errors.New("tools: transient error")

	// ErrPermanent flags non-recoverable downstream failures.
	ErrPermanent = errors.New("tools: permanent error")

	// ErrTimeout flags a deadline the framework itself imposed — a tool-call
	// budget, a query timeout, a poll ceiling. It is deliberately distinct
	// from context.DeadlineExceeded, which any enclosing context also
	// produces: a layer that sets its own deadline needs to know whether the
	// deadline that fired was *its own* before reporting one. Attach it with
	// DeadlineCause and test with TimedOut.
	ErrTimeout = errors.New("tools: deadline exceeded")
)

// DeadlineCause builds the cause value to hand to context.WithTimeoutCause,
// naming the budget that will have elapsed. what should read as the thing
// being bounded ("tool call", "sql query"), not the tool's name.
//
//	ctx, cancel := context.WithTimeoutCause(ctx, d, tools.DeadlineCause("sql query", d))
//
// The returned error satisfies errors.Is(err, ErrTimeout) and carries the
// duration in its message, so a model-facing report can name the budget
// without the reporting site holding it.
func DeadlineCause(what string, d time.Duration) error {
	return fmt.Errorf("%w: %s exceeded its %s budget", ErrTimeout, what, d)
}

// TimedOut reports whether ctx is done because a deadline set with
// DeadlineCause elapsed. It answers "was it *my* deadline?" — an enclosing
// context that expires or is cancelled first leaves its own cause in place,
// so this returns false and the caller correctly reports cancellation
// instead of claiming its own timeout fired.
//
// Prefer this over comparing ctx.Err() to context.DeadlineExceeded, which
// cannot tell the two apart.
func TimedOut(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrTimeout)
}

// ClassifyError returns the classification sentinel that err unwraps to,
// or nil when err matches none of them. Consumers use this for concise
// branching in error handlers:
//
//	switch tools.ClassifyError(err) {
//	case tools.ErrUser:      // show to user, don't retry
//	case tools.ErrTransient: // maybe retry with backoff
//	case tools.ErrPermanent: // give up
//	default:                 // unknown — default conservative handling
//	}
//
// Passing nil returns nil.
func ClassifyError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUser):
		return ErrUser
	case errors.Is(err, ErrTransient):
		return ErrTransient
	case errors.Is(err, ErrPermanent):
		return ErrPermanent
	default:
		return nil
	}
}
