package agent

import (
	"context"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// DynamicContextFunc returns per-request text to append to the message list
// at LLM-call time. Return "" to skip. The returned text is NOT persisted to
// session history — distinguishing this from mutating SessionManager.SystemPrompt.
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
// Prompt-cache placement: the text is appended as a NEW message at the tail
// of the message list, not inserted into the system message. This keeps the
// historical prefix (system + persisted turns) byte-identical across dynamic
// changes, so an Anthropic CacheHint on the last persisted user message
// stays valid regardless of how often the dynamic text rotates. The dynamic
// tail itself is fresh on every call, which is the correct behavior — fresh
// content was never going to be cached anyway.
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
// when AgentLoop.DynamicContext is set and returns a non-empty string. The
// addition is appended as a new user-role message at the tail, which keeps
// the historical prefix byte-identical across turns so prompt-cache hits
// survive dynamic-text rotation. Input slice is never mutated; sentinel
// check ensures idempotency across retries within the same iteration.
func (al *AgentLoop) withDynamicContext(ctx context.Context, sessionKey string, msgs []history.Message) []history.Message {
	if al.DynamicContext == nil {
		return msgs
	}
	addition := al.DynamicContext(ctx, sessionKey)
	if addition == "" {
		return msgs
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, dynamicContextSentinel) {
			return msgs
		}
	}
	tagged := dynamicContextSentinel + "\n" + addition
	out := make([]history.Message, len(msgs), len(msgs)+1)
	copy(out, msgs)
	return append(out, history.Message{Role: "user", Content: tagged})
}
