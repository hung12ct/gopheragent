package agent

import (
	"fmt"
	"unicode/utf8"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// SoftTrimThreshold is the max rune length of a tool result before it gets pruned.
const SoftTrimThreshold = 4000

// RetentionHead is the number of runes to keep from the start of a pruned message.
const RetentionHead = 1500

// RetentionTail is the number of runes to keep from the end of a pruned message.
const RetentionTail = 1500

// OutlierTrimThreshold is the max rune length before an entire message is discarded.
// (~50k chars is around 12k tokens, often indicating memory leaks or raw database dumps).
const OutlierTrimThreshold = 50000

// ToolArgTruncateLen is the rune length kept by TruncateToolArguments when a
// tool result is force-truncated as a last-resort token-budget defense.
const ToolArgTruncateLen = 500

// runeSlice returns s[start:end] measured in runes, safe for multi-byte UTF-8.
// Returns "" when the range is empty, inverted, or entirely out of bounds.
// When end exceeds the rune count the slice is clamped to the string's end.
func runeSlice(s string, start, end int) string {
	if end <= start || start < 0 {
		return ""
	}
	var byteStart, byteEnd int
	startFound := false
	runeIdx := 0
	for i := range s {
		if runeIdx == start {
			byteStart = i
			startFound = true
		}
		if runeIdx == end {
			byteEnd = i
			return s[byteStart:byteEnd]
		}
		runeIdx++
	}
	if !startFound {
		return ""
	}
	return s[byteStart:]
}

// PruneContextMessages acts as Layer 1 Defense.
// It performs a soft trim by cutting the middle of excessively long tool responses
// but strictly protects the last 'protectedEnds' messages from any modification.
// All slicing is rune-safe to avoid corrupting multi-byte UTF-8 (CJK, emoji).
func PruneContextMessages(msgs []history.Message, protectedEnds int) []history.Message {
	if len(msgs) == 0 {
		return msgs
	}

	result := make([]history.Message, 0, len(msgs))

	protectStartIdx := len(msgs) - protectedEnds
	if protectStartIdx < 0 {
		protectStartIdx = 0
	}

	for i, msg := range msgs {
		if (msg.Role == "tool" || msg.Role == "assistant") && i < protectStartIdx {
			runeLen := utf8.RuneCountInString(msg.Content)

			if runeLen > OutlierTrimThreshold {
				msg.Content = fmt.Sprintf("\n[System: Outlier Payload Truncated] The tool returned %d characters which exceeds the safety threshold of %d. Payload was completely discarded to avoid context explosion. Retry with tighter parameters.\n", runeLen, OutlierTrimThreshold)
				result = append(result, msg)
				continue
			}

			if runeLen > SoftTrimThreshold {
				head := runeSlice(msg.Content, 0, RetentionHead)
				tail := runeSlice(msg.Content, runeLen-RetentionTail, runeLen)
				omitted := runeLen - RetentionHead - RetentionTail

				msg.Content = fmt.Sprintf("%s\n\n... [%d chars truncated] ...\n\n%s", head, omitted, tail)
			}
		}
		result = append(result, msg)
	}

	return result
}

// hasDanglingToolCalls walks msgs once and returns true if any assistant
// tool_call lacks a matching tool response. Used as a cheap gate so the
// allocating rebuild path in PatchDanglingToolCalls only runs when needed.
func hasDanglingToolCalls(msgs []history.Message) bool {
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		responded := make(map[string]struct{}, len(m.ToolCalls))
		j := i + 1
		for j < len(msgs) {
			if msgs[j].Role == "assistant" || msgs[j].Role == "user" {
				break
			}
			if msgs[j].Role == "tool" && msgs[j].ToolCallID != "" {
				responded[msgs[j].ToolCallID] = struct{}{}
			}
			j++
		}
		for _, tc := range m.ToolCalls {
			if _, ok := responded[tc.ID]; !ok {
				return true
			}
		}
		i = j - 1
	}
	return false
}

// PatchDanglingToolCalls scans message history and injects synthetic tool
// responses for any tool_call that never received a reply. Without this,
// provider APIs (Anthropic / OpenAI) reject the request with a 400/500
// because every tool_use must be paired with a tool_result before the next
// user/assistant turn.
//
// Returns the input slice unchanged when no dangling call is detected (no
// allocation), so the per-turn call from runLogicLoop is zero-cost in the
// healthy case. When patching is required, synthetic messages are appended
// *after* any existing tool responses for the same assistant turn so call
// order matches the source tool_calls order.
func PatchDanglingToolCalls(msgs []history.Message) []history.Message {
	if len(msgs) == 0 || !hasDanglingToolCalls(msgs) {
		return msgs
	}
	patched := make([]history.Message, 0, len(msgs))

	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		patched = append(patched, m)

		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}

		// Consume the contiguous tool-response block following this assistant turn.
		responded := make(map[string]bool)
		j := i + 1
		for j < len(msgs) {
			if msgs[j].Role == "assistant" || msgs[j].Role == "user" {
				break
			}
			if msgs[j].Role == "tool" && msgs[j].ToolCallID != "" {
				responded[msgs[j].ToolCallID] = true
			}
			patched = append(patched, msgs[j])
			j++
		}

		// Append synthetic responses for any unmatched tool_call.
		for _, tc := range m.ToolCalls {
			if !responded[tc.ID] {
				patched = append(patched, history.Message{
					Role:       "tool",
					Content:    "[System] Tool call cancelled due to system interruption.",
					ToolCallID: tc.ID,
					IsError:    true,
				})
			}
		}

		i = j - 1 // skip the block we already emitted
	}
	return patched
}

// TruncateToolArguments forcefully truncates tool outputs to prevent context
// window overflow when running tight on token budget. Non-tool messages pass
// through unchanged. Truncation is rune-safe.
func TruncateToolArguments(msgs []history.Message) []history.Message {
	if len(msgs) == 0 {
		return msgs
	}
	result := make([]history.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Role == "tool" {
			runeLen := utf8.RuneCountInString(msg.Content)
			if runeLen > ToolArgTruncateLen {
				msg.Content = runeSlice(msg.Content, 0, ToolArgTruncateLen) + "\n... (output truncated by system to save tokens)"
			}
		}
		result = append(result, msg)
	}
	return result
}
