package tools

import "errors"

// Error classification sentinels. Tool authors wrap their real error with
// one of these to let the agent loop and clients distinguish the failure
// category:
//
//   - ErrUser       — the caller supplied bad input; show the message to
//                     the user, do not retry.
//   - ErrTransient  — transient downstream failure (rate limit, 5xx,
//                     network timeout); retrying later may succeed.
//   - ErrPermanent  — non-recoverable downstream failure (bad config,
//                     permanent 4xx from an external API); retries will
//                     keep failing.
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
)

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
