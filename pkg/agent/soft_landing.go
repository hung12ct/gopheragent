package agent

import (
	"strings"

	"github.com/hung12ct/gopheragent/pkg/history"
)

const softLandingSentinel = "[soft-landing]"

const softLandingHint = softLandingSentinel +
	" You are approaching the iteration limit for this run. Stop calling tools and produce the final answer to the user now."

// withSoftLandingHint returns msgs augmented with a transient system nudge
// on the final two iterations of a run. Returned only — never persisted —
// so saved session history stays clean across turns. Idempotent.
func withSoftLandingHint(iteration, maxIters int, msgs []history.Message) []history.Message {
	if maxIters <= 0 || iteration < maxIters-2 {
		return msgs
	}
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, softLandingSentinel) {
			return msgs
		}
	}
	return append(msgs, history.Message{Role: "system", Content: softLandingHint})
}
