package history

// snapToSafeBoundary returns an index <= atIndex such that msgs[:result] is a
// self-contained prefix: it never ends mid tool-call / tool-result group.
//
// Specifically, the returned prefix satisfies both:
//   - no trailing assistant message has unresolved ToolCalls (dangling calls),
//   - no trailing tool message lacks its matching assistant (orphan results).
//
// When the input atIndex is already safe, it is returned unchanged. When it is
// not, the index is walked backward past the offending boundary.
func snapToSafeBoundary(msgs []Message, atIndex int) int {
	if atIndex < 0 {
		return 0
	}
	end := atIndex
	if end > len(msgs) {
		end = len(msgs)
	}

	for end > 0 {
		last := msgs[end-1]

		// Dangling assistant with tool_calls — drop it.
		if last.Role == "assistant" && len(last.ToolCalls) > 0 {
			end--
			continue
		}

		// Trailing tool result — ensure every ToolCall from the preceding
		// assistant is resolved inside the prefix. Otherwise drop the whole
		// assistant/tool-results group.
		if last.Role == "tool" {
			asst := -1
			for i := end - 2; i >= 0; i-- {
				r := msgs[i].Role
				if r == "assistant" && len(msgs[i].ToolCalls) > 0 {
					asst = i
					break
				}
				if r == "user" || r == "system" {
					break
				}
			}
			if asst < 0 {
				// orphan tool result, no matching assistant
				end--
				continue
			}
			required := make(map[string]struct{}, len(msgs[asst].ToolCalls))
			for _, tc := range msgs[asst].ToolCalls {
				required[tc.ID] = struct{}{}
			}
			for i := asst + 1; i < end; i++ {
				if msgs[i].Role == "tool" {
					delete(required, msgs[i].ToolCallID)
				}
			}
			if len(required) == 0 {
				return end // safe: full group included
			}
			end = asst
			continue
		}

		return end
	}
	return 0
}
