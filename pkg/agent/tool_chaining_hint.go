package agent

import (
	"strings"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// toolChainingSentinel is the substring every variant of the hint contains.
// Used to detect whether a user-supplied system prompt already documents the
// chaining syntax, in which case we skip auto-injection.
const toolChainingSentinel = "<output_of:"

// ToolChainingHint returns the system-prompt snippet that teaches the LLM to
// chain tool calls via <output_of:<tool_call_id>.<field>> references. The
// AgentLoop appends this automatically to the system prompt on each turn when
// at least two tools are registered (disable via DisableToolChainingHint).
//
// Exposed so callers who prefer to own the full system prompt can paste this
// text verbatim — the loop detects it by sentinel substring and skips the
// duplicate injection.
func ToolChainingHint() string {
	return "When one tool's output is needed as an argument to another tool in the same turn, embed a reference of the form <output_of:<tool_call_id>.<field>> directly in the dependent tool's arguments instead of waiting for a second round-trip. The system runs dependencies first, substitutes the JSON value in place, and then runs the dependent call. " +
		`Example: [{"id":"t1","name":"fetch_user","args":{"id":1}},{"id":"t2","name":"fetch_orders","args":{"user_id":<output_of:t1.user_id>}}]. ` +
		"Use the exact tool_call_id you assigned (e.g. \"t1\"). Omit the field suffix (<output_of:t1>) to inject the full output object. If a call has no dependencies, emit it normally — calls in the same wave still run in parallel."
}

// withToolChainingHint returns msgs augmented with the tool-chaining hint
// when it is not already present. The input slice is never mutated; when no
// augmentation is needed the same slice is returned. The hint is merged into
// the first system message (or prepended as a new system message if none
// exists) so providers that expect a single consolidated system prompt keep
// working unchanged.
//
// Injection is skipped when:
//   - the loop has DisableToolChainingHint == true, or
//   - fewer than two tools are registered (no reason to chain), or
//   - any existing system message already contains toolChainingSentinel.
func (al *AgentLoop) withToolChainingHint(msgs []history.Message) []history.Message {
	if al.DisableToolChainingHint {
		return msgs
	}
	if al.Tools == nil || al.Tools.Len() < 2 {
		return msgs
	}
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, toolChainingSentinel) {
			return msgs
		}
	}

	hint := ToolChainingHint()
	out := make([]history.Message, len(msgs))
	copy(out, msgs)
	if len(out) > 0 && out[0].Role == "system" {
		out[0].Content = out[0].Content + "\n\n" + hint
		return out
	}
	return append([]history.Message{{Role: "system", Content: hint}}, out...)
}
