package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// The request the loop sends is not stored history: deriveRequestMessages
// applies the budget policy to it, and buildMsgsForLLM then layers on the
// soft-landing, memory-note, tool-chaining, plan-mode and dynamic-context
// injections. Two properties keep that pipeline honest, and both are
// currently held by convention alone:
//
//  1. Stored history is read-only along the request path. Every stage
//     returns a derived slice; none may write through to the caller's
//     messages, which are the source of truth for what gets persisted.
//  2. The derivation is pure. Re-running it on the same stored history
//     yields the same messages, so what a request carried stays
//     reconstructable from the log plus the declared injections.
//
// Property 1 has been violated before — buildMsgsForLLM copies before
// stamping CacheHint precisely because the stamp used to leak into the
// caller's session-loaded slice. A comment cannot hold that; this can.

// RequestViolationFunc receives a description of a broken request-path
// invariant. It runs synchronously on the loop goroutine, so keep it cheap;
// panicking or blocking here stalls the turn.
type RequestViolationFunc func(ctx context.Context, err error)

// requestSnapshot is a copy of stored history taken before the request
// pipeline runs, kept so the loop can prove the pipeline did not write
// through to it.
type requestSnapshot struct {
	stored []history.Message
}

// snapshotRequest copies the fields the request path could plausibly
// rewrite. It is nil-cheap: with no invariant configured, no copy is taken
// and the whole mechanism costs one nil check per iteration.
//
// Parts are compared by length rather than deep-copied: the pruning and
// injection stages only ever rewrite Content, and copying media bytes on
// every call would cost more than the check is worth.
func (al *AgentLoop) snapshotRequest(stored []history.Message) *requestSnapshot {
	if al.RequestInvariant == nil {
		return nil
	}
	cp := make([]history.Message, len(stored))
	copy(cp, stored)
	return &requestSnapshot{stored: cp}
}

// checkRequestInvariant verifies both properties and reports any violation
// through al.RequestInvariant. snap is nil when the invariant is disabled,
// which makes this a single branch on the hot path.
//
// sent is the message list handed to the provider; stored is the loop's
// live history slice after the call returned.
func (al *AgentLoop) checkRequestInvariant(ctx context.Context, snap *requestSnapshot, stored, sent []history.Message) {
	if snap == nil || al.RequestInvariant == nil {
		return
	}
	if err := storedUnchanged(snap.stored, stored); err != nil {
		al.RequestInvariant(ctx, fmt.Errorf("agent: request path wrote through to stored history: %w", err))
	}
	if err := derivationReproduces(snap.stored, al.MaxTokenBudget, sent); err != nil {
		al.RequestInvariant(ctx, fmt.Errorf("agent: request diverges from a re-derivation of stored history: %w", err))
	}
}

// storedUnchanged reports whether the request pipeline left the caller's
// history slice exactly as it found it.
func storedUnchanged(before, after []history.Message) error {
	if len(before) != len(after) {
		return fmt.Errorf("message count changed from %d to %d", len(before), len(after))
	}
	for i := range before {
		if err := sameMessage(before[i], after[i]); err != nil {
			return fmt.Errorf("message %d (%s): %w", i, before[i].Role, err)
		}
	}
	return nil
}

// derivationReproduces re-runs the pure derivation over the pre-call
// snapshot and checks that the conversation the provider received is still
// exactly the derived one.
//
// System messages are excluded: they are the declared framing surface, and
// four separate stages legitimately prepend to or extend them. What must
// survive untouched is the conversation itself — every derived user,
// assistant and tool message, in order, byte for byte. A request may also
// carry the dynamic-context injection, which is admitted by its sentinel;
// anything else appearing in conversation position is content reaching the
// model that no re-derivation can account for.
func derivationReproduces(snapshot []history.Message, maxTokenBudget int, sent []history.Message) error {
	want := conversationOf(deriveRequestMessages(snapshot, maxTokenBudget).messages)
	got := conversationOf(sent)

	next := 0
	for _, m := range got {
		if next < len(want) {
			if err := sameMessage(want[next], m); err == nil {
				next++
				continue
			}
		}
		if strings.Contains(m.Content, dynamicContextSentinel) {
			continue
		}
		if next >= len(want) {
			return fmt.Errorf("request carries an extra %s message not derived from stored history and not a declared injection", m.Role)
		}
		return fmt.Errorf("message %d: expected the derived %s message, got a %s message that diverges: %w",
			next, want[next].Role, m.Role, sameMessage(want[next], m))
	}
	if next != len(want) {
		return fmt.Errorf("request dropped %d of %d derived conversation messages", len(want)-next, len(want))
	}
	return nil
}

// conversationOf returns the non-system messages: the part of a request
// that must be reconstructable from stored history.
func conversationOf(msgs []history.Message) []history.Message {
	out := make([]history.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != "system" {
			out = append(out, m)
		}
	}
	return out
}

// sameMessage compares the fields that decide what a provider receives and
// what a replay would reconstruct. Deliberately not reflect.DeepEqual:
// Parts can carry megabytes of media, and CacheHint is stamped on the
// request copy by design.
func sameMessage(a, b history.Message) error {
	switch {
	case a.Role != b.Role:
		return fmt.Errorf("role %q became %q", a.Role, b.Role)
	case a.Content != b.Content:
		return fmt.Errorf("content changed (%d chars became %d)", len(a.Content), len(b.Content))
	case a.ToolCallID != b.ToolCallID:
		return fmt.Errorf("tool_call_id %q became %q", a.ToolCallID, b.ToolCallID)
	case len(a.ToolCalls) != len(b.ToolCalls):
		return fmt.Errorf("tool_calls count %d became %d", len(a.ToolCalls), len(b.ToolCalls))
	case len(a.Parts) != len(b.Parts):
		return fmt.Errorf("parts count %d became %d", len(a.Parts), len(b.Parts))
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i].ID != b.ToolCalls[i].ID || a.ToolCalls[i].Arguments != b.ToolCalls[i].Arguments {
			return fmt.Errorf("tool_call %d changed", i)
		}
	}
	return nil
}
