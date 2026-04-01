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

// runeSlice returns s[0:n] measured in runes, safe for multi-byte UTF-8.
func runeSlice(s string, start, end int) string {
	var byteStart, byteEnd int
	runeIdx := 0
	for i := range s {
		if runeIdx == start {
			byteStart = i
		}
		if runeIdx == end {
			byteEnd = i
			return s[byteStart:byteEnd]
		}
		runeIdx++
	}
	if runeIdx == end {
		return s[byteStart:]
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

				msg.Content = fmt.Sprintf("%s\n\n... [... %d chars omitted by GopherAgent Pruning ...] ...\n\n%s", head, omitted, tail)
			}
		}
		result = append(result, msg)
	}

	return result
}
