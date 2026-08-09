package agent

import (
	"context"
	"fmt"
	"iter"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// Regenerate rewinds sessionKey's persisted history to just before the last
// user message and replays that user message through a fresh iteration. The
// returned iterator yields the replay stream, with EventTypeRegenerated as
// the first frame so UIs can mark the previous assistant bubble as
// superseded before replacement content arrives.
//
// Returns ErrNothingToRegenerate (and a nil iterator) without touching
// history when no user message exists. The truncation is persisted via
// SaveHistory before the loop starts so a crash mid-replay never leaves a
// hybrid history. BeforeHooks fire with the recovered user message's
// Content, matching the RunIteration contract. Per-session counters
// (BudgetTracker, MaxToolCallsPerSession) are NOT rewound automatically.
//
// Usage:
//
//	seq, err := loop.Regenerate(ctx, sessionKey)
//	if err != nil { return err }
//	for ev := range seq { /* ... */ }
func (al *AgentLoop) Regenerate(ctx context.Context, sessionKey string) (iter.Seq[StreamEvent], error) {
	existing, err := al.Sessions.History(ctx, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("agent: regenerate: load history: %w", err)
	}
	userIdx := lastUserIndex(existing)
	if userIdx < 0 {
		return nil, ErrNothingToRegenerate
	}
	userMsg := existing[userIdx]
	prevAsstIdx := lastAssistantIndex(existing)
	truncatedAt := history.SafeTruncate(existing, userIdx)
	truncated := append([]history.Message(nil), existing[:truncatedAt]...)
	al.saveSession(ctx, sessionKey, truncated)

	firstFrame := regeneratedEvent(prevAsstIdx, truncatedAt)
	emitThoughts := al.EmitThoughts
	return func(yield func(StreamEvent) bool) {
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if !yield(firstFrame) {
			return
		}
		internalChan := make(chan StreamEvent, runIterationStreamBuffer)
		go al.runLogicLoop(runCtx, sessionKey, userMsg, internalChan)
		yieldEvents(internalChan, yield, emitThoughts, cancel)
	}, nil
}

// Continue resumes the agent loop on sessionKey's current persisted history
// without appending a new user message. Use it when the previous run hit
// MaxIters / MaxToolCallsPerSession / a HITL timeout / a user-issued Stop
// and the user wants the agent to keep going from where it left off.
//
// Returns ErrNothingToContinue when the session's last persisted message is
// a clean final-assistant turn (no dangling tool calls and no awaiting tool
// results); the caller should send a new user message instead.
// patchDanglingToolCalls is applied to the live history before iteration so
// any half-finished tool wave from the prior run is sealed.
//
// EventTypeContinued is yielded as the first frame so UIs can attach
// subsequent content to the existing assistant bubble.
func (al *AgentLoop) Continue(ctx context.Context, sessionKey string) (iter.Seq[StreamEvent], error) {
	existing, err := al.Sessions.History(ctx, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("agent: continue: load history: %w", err)
	}
	if !canContinue(existing) {
		return nil, ErrNothingToContinue
	}
	resumeFrom := len(existing) - 1

	firstFrame := continuedEvent(resumeFrom)
	emitThoughts := al.EmitThoughts
	return func(yield func(StreamEvent) bool) {
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if !yield(firstFrame) {
			return
		}
		internalChan := make(chan StreamEvent, runIterationStreamBuffer)
		go al.continueLogicLoop(runCtx, sessionKey, internalChan)
		yieldEvents(internalChan, yield, emitThoughts, cancel)
	}, nil
}

// continueLogicLoop drives an iteration sequence on sessionKey's existing
// persisted history. Mirrors runLogicLoop but skips the user-message append
// and the SessionCreated emission. patchDanglingToolCalls makes the live
// history safe before the next LLM call.
func (al *AgentLoop) continueLogicLoop(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent) {
	defer close(streamChan)
	ctx = WithSessionKey(ctx, sessionKey)

	// Mirror runLogicLoop: per-Run cost accumulator fires on every
	// terminal exit. Without this,
	// Regenerate and Continue Runs would never emit RunCostEvent and
	// adopters tracking billing would miss them silently.
	var emitCost func()
	ctx, emitCost = al.installRunCostAccumulator(ctx, sessionKey, streamChan)
	defer emitCost()

	// Mirror runLogicLoop so a resumed Run reports partial-success state
	// the same way a fresh one does.
	var sweepDegraded func()
	ctx, sweepDegraded = al.installDegradationAccumulator(ctx, sessionKey, streamChan)
	defer sweepDegraded()

	msgs, err := al.Sessions.History(ctx, sessionKey)
	if err != nil {
		al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("agent: continue: load history: %w", err)))
		return
	}
	msgs = patchDanglingToolCalls(msgs)
	if err := al.Sessions.SaveHistory(ctx, sessionKey, msgs); err != nil {
		al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("agent: continue: save history: %w", err)))
		return
	}

	al.iterateMessages(ctx, sessionKey, streamChan, msgs)
}

// regeneratedEvent builds the typed transition frame emitted at the start of
// a Regenerate stream.
func regeneratedEvent(previousAssistantIndex, truncatedAt int) StreamEvent {
	return Event(RegeneratedEvent{PreviousAssistantIndex: previousAssistantIndex, TruncatedAt: truncatedAt})
}

// continuedEvent builds the typed transition frame emitted at the start of a
// Continue stream.
func continuedEvent(continuedFromIndex int) StreamEvent {
	return Event(ContinuedEvent{ContinuedFromIndex: continuedFromIndex})
}

// lastUserIndex returns the highest index of a "user" message, or -1.
func lastUserIndex(msgs []history.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return -1
}

// lastAssistantIndex returns the highest index of an "assistant" message, or
// -1 when none exists. Used to populate RegeneratedEvent.PreviousAssistantIndex
// so UIs can mark the superseded bubble without scanning history themselves.
func lastAssistantIndex(msgs []history.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return i
		}
	}
	return -1
}

// canContinue reports whether the session has unfinished work that warrants
// a Continue resume. Clean final-assistant endings (no dangling tool_calls,
// no awaiting tool results) return false so the caller gets
// ErrNothingToContinue and prompts the user for a new message instead of
// re-running the LLM on a settled conversation.
func canContinue(msgs []history.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	switch last.Role {
	case "assistant":
		// A trailing assistant turn with dangling tool_calls means the
		// prior iteration was interrupted mid-wave; resuming reuses
		// patchDanglingToolCalls to seal it before the next LLM call.
		// A trailing assistant turn with no tool_calls is a clean final.
		return len(last.ToolCalls) > 0
	case "user", "tool":
		// User awaiting a response, or a tool result that never made it
		// back to the model — both legitimate resume points.
		return true
	default:
		// System-only history (or unknown roles) — nothing meaningful to resume.
		return false
	}
}
