package agent

import "github.com/hung12ct/gopheragent/pkg/history"

// requestPolicy is the configuration deriveRequestMessages reads. Keeping it
// a value lets the request invariant re-run the derivation with exactly the
// settings the loop used.
type requestPolicy struct {
	tokenBudget   int
	historyWindow int
}

func (al *AgentLoop) requestPolicy() requestPolicy {
	return requestPolicy{tokenBudget: al.MaxTokenBudget, historyWindow: al.historyWindow()}
}

// historyWindowStart returns the stored index where the windowed
// conversation begins, plus the length of the leading system block that is
// always kept. start == sys means nothing is dropped.
//
// The cut moves in steps of window/2 rather than one message per turn, so
// the request prefix stays byte-identical for many turns and provider
// prompt caches keep hitting; a sliding window would invalidate the cache
// on every call. Between cuts the conversation holds window/2..window
// messages.
//
// The cut then snaps back to the nearest user message at or before it, so
// no tool result is orphaned from its assistant tool call and the request
// never opens mid-turn. That makes window a soft cap: a single turn longer
// than window is kept whole. When no user message lies in range the
// window drops nothing.
func historyWindowStart(stored []history.Message, window int) (start, sys int) {
	sys = leadingSystem(stored)
	if window <= 0 || len(stored)-sys <= window {
		return sys, sys
	}
	step := max(window/2, 1)
	excess := len(stored) - sys - window
	cut := sys + (excess+step-1)/step*step
	for i := cut; i > sys; i-- {
		if stored[i].Role == "user" {
			return i, sys
		}
	}
	return sys, sys
}

// applyHistoryWindow returns the leading system block followed by
// stored[start:]. It never writes through to stored: with no system block
// it reslices, otherwise it copies into a fresh slice.
func applyHistoryWindow(stored []history.Message, start, sys int) []history.Message {
	if start == sys {
		return stored
	}
	if sys == 0 {
		return stored[start:]
	}
	out := make([]history.Message, 0, sys+len(stored)-start)
	out = append(out, stored[:sys]...)
	return append(out, stored[start:]...)
}

// rebaseRefs maps ContextRef indexes computed on the windowed view back to
// positions in stored history, so a trace stays joinable to Sessions.History.
func rebaseRefs(refs []ContextRef, v requestView, start, sys int) {
	for i := range refs {
		idx := refs[i].Index
		if idx >= sys {
			idx += start - sys
		}
		refs[i].Index = v.storedIndex(idx)
	}
}
