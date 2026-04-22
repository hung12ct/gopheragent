package agent

import (
	"context"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// DynamicContextFunc returns per-request text to append to the system prompt
// at LLM-call time. Return "" to skip. The returned text is NOT persisted to
// session history — same semantics as plan-mode and tool-chaining hints, and
// the key distinction from mutating SessionManager.SystemPrompt.
//
// Typical use cases: today's date, per-session feature flags, dynamic RAG
// snippets, per-org prompt variants.
//
// Hot-path warning: called on every LLM invocation, which includes retries,
// Reflect rounds, and every ReAct iteration within a single turn. Do no
// synchronous I/O here — close over pre-computed state or cache externally
// with a TTL. A function that does a DB round trip will multiply per-turn
// latency by the iteration count.
//
// Stability: callers should return values that are stable across the
// iterations of a single user turn. A timestamp that changes every second
// defeats prompt caching and can confuse the model across tool-call rounds.
//
// Prompt-cache interaction: the returned text is appended to the system
// message's Content. Cached prefixes up to the splice point still hit, but
// if Message.CacheHint is placed on the system message itself, every change
// in the dynamic tail invalidates the cache. Place CacheHint on a later
// message (typically the last user turn) to keep the cache warm.
//
// Sub-agent scope: DynamicContext is per-AgentLoop. Sub-agents do not
// inherit it from the parent — set it on each sub-agent's loop if you want
// the same context inside them.
type DynamicContextFunc func(ctx context.Context, sessionKey string) string

// dynamicContextSentinel marks an already-injected block so we never
// double-append on retries within the same iteration. The sentinel is an
// HTML comment so it is inert to any LLM that surfaces it verbatim.
const dynamicContextSentinel = "<!-- dynamic-context -->"

// withDynamicContext returns msgs augmented with the dynamic context block
// when AgentLoop.DynamicContext is set and returns a non-empty string.
// Input slice is never mutated; sentinel check ensures idempotency across
// retries within the same iteration.
func (al *AgentLoop) withDynamicContext(ctx context.Context, sessionKey string, msgs []history.Message) []history.Message {
	if al.DynamicContext == nil {
		return msgs
	}
	addition := al.DynamicContext(ctx, sessionKey)
	if addition == "" {
		return msgs
	}
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, dynamicContextSentinel) {
			return msgs
		}
	}
	tagged := dynamicContextSentinel + "\n" + addition
	out := make([]history.Message, len(msgs))
	copy(out, msgs)
	if len(out) > 0 && out[0].Role == "system" {
		out[0].Content = out[0].Content + "\n\n" + tagged
		return out
	}
	return append([]history.Message{{Role: "system", Content: tagged}}, out...)
}
