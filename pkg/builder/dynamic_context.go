package builder

import (
	"context"

	"github.com/hung12ct/gopheragent/pkg/agent"
)

// DynamicKB lifts a KB-document producer into an agent.DynamicContextFunc by
// rendering the documents through FormatKnowledgeBase. The output is the
// same <knowledge_base><file path="…">…</file></knowledge_base> block that
// the builder uses at build time, so static KB loaded from YAML and dynamic
// KB from this adapter are indistinguishable to the LLM.
//
// The block is delivered as a tail message (not folded into the system
// prompt) so the historical prefix stays byte-identical across dynamic
// changes — static KB from YAML still benefits from Anthropic prompt
// caching regardless of how often the dynamic KB rotates.
//
// Returning an empty slice (or nil) yields an empty string, which causes
// agent.withDynamicContext to skip augmentation for that call.
//
// All contracts documented on agent.DynamicContextFunc — hot-path budget,
// turn-level stability, and prompt-cache placement — apply unchanged.
func DynamicKB(fn func(ctx context.Context, sessionKey string) []KBDocument) agent.DynamicContextFunc {
	return func(ctx context.Context, sessionKey string) string {
		return FormatKnowledgeBase(fn(ctx, sessionKey))
	}
}
