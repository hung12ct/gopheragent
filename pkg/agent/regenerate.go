package agent

import (
	"context"
	"fmt"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// Regenerate rewinds sessionKey's persisted history to just before the last
// user message and replays that user message through a fresh iteration.
// It is the library-owned equivalent of the "re-run my last turn" affordance
// every chat UI eventually ships — adopters no longer have to walk history
// backwards manually (and risk splitting a tool_use/tool_result pair, the
// exact shape that produces Anthropic 400s).
//
// Stream lifecycle matches RunIterationStream: the call returns immediately
// after kicking off the loop goroutine; the library owns streamChan and
// closes it exactly once when the run terminates. EventTypeRegenerated is
// the first frame emitted on the channel so UIs can mark the previous
// assistant bubble as superseded before replacement content arrives.
//
// Returns ErrNothingToRegenerate without touching history or the stream
// when no user message exists. The truncation is persisted via Save before
// the loop starts so a crash mid-replay never leaves a hybrid history.
//
// BeforeHooks fire with the recovered user message's Content, matching the
// RunIteration contract. Per-session counters (BudgetTracker,
// MaxToolCallsPerSession) are NOT rewound automatically — adopters that
// track per-turn token usage themselves should call BudgetTracker.Reset or
// a custom rewind before invoking Regenerate.
func (al *AgentLoop) Regenerate(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent) error {
	existing := al.Sessions.GetHistory(ctx, sessionKey)
	userIdx := lastUserIndex(existing)
	if userIdx < 0 {
		close(streamChan)
		return ErrNothingToRegenerate
	}
	userMsg := existing[userIdx]
	prevAsstIdx := lastAssistantIndex(existing)
	truncatedAt := history.SafeTruncate(existing, userIdx)
	truncated := append([]history.Message(nil), existing[:truncatedAt]...)
	al.saveSession(ctx, sessionKey, truncated)

	internalChan := make(chan StreamEvent, runIterationStreamBuffer)
	emitThoughts := al.EmitThoughts
	go al.transitionRelayer(ctx, streamChan, internalChan, regeneratedEvent(prevAsstIdx, truncatedAt), emitThoughts)
	go al.runLogicLoop(ctx, sessionKey, userMsg, internalChan)
	return nil
}

// Continue resumes the agent loop on sessionKey's current persisted history
// without appending a new user message. Use it when the previous run hit
// MaxIters / MaxToolCallsPerSession / a HITL timeout / a user-issued Stop
// and the user wants the agent to keep going from where it left off —
// sending a synthetic "continue" user message would burn a budget slot and
// pollute the picker with a fake turn.
//
// Stream lifecycle matches RunIterationStream. The first emitted frame is
// EventTypeContinued. Returns ErrNothingToContinue when the session's last
// persisted message is a clean final-assistant turn (no dangling tool calls
// and no awaiting tool results); in that state the caller should send a new
// user message instead.
//
// PatchDanglingToolCalls is applied to the live history before iteration so
// any half-finished tool wave from the prior run is sealed with synthetic
// error results — without this, providers reject the next call with a
// tool_use-without-tool_result 400.
func (al *AgentLoop) Continue(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent) error {
	existing := al.Sessions.GetHistory(ctx, sessionKey)
	if !canContinue(existing) {
		close(streamChan)
		return ErrNothingToContinue
	}
	resumeFrom := len(existing) - 1

	internalChan := make(chan StreamEvent, runIterationStreamBuffer)
	emitThoughts := al.EmitThoughts
	go al.transitionRelayer(ctx, streamChan, internalChan, continuedEvent(resumeFrom), emitThoughts)
	go al.continueLogicLoop(ctx, sessionKey, internalChan)
	return nil
}

// continueLogicLoop drives an iteration sequence on sessionKey's existing
// persisted history. Mirrors runLogicLoop but skips the user-message append
// and the SessionCreated emission (the session, by definition, already
// exists). PatchDanglingToolCalls makes the live history safe before the
// next LLM call.
func (al *AgentLoop) continueLogicLoop(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent) {
	defer close(streamChan)
	ctx = WithSessionKey(ctx, sessionKey)

	msgs := al.Sessions.GetHistory(ctx, sessionKey)
	msgs = PatchDanglingToolCalls(msgs)
	al.Sessions.SetHistory(ctx, sessionKey, msgs)

	al.iterateMessages(ctx, sessionKey, streamChan, msgs)
}

// transitionRelayer streams events from internalChan to streamChan, prepending
// firstFrame (EventTypeRegenerated / EventTypeContinued) so UIs see the
// transition signal before any replacement content. Mirrors the relayer
// shape inside RunIterationStreamMessage — thought suppression, ctx-cancel
// terminal forwarding, library-owned close.
func (al *AgentLoop) transitionRelayer(ctx context.Context, streamChan chan<- StreamEvent, internalChan <-chan StreamEvent, firstFrame StreamEvent, emitThoughts bool) {
	defer close(streamChan)
	select {
	case streamChan <- firstFrame:
	case <-ctx.Done():
		drainBestEffort(streamChan, internalChan)
		return
	}
	for ev := range internalChan {
		if ev.Type == EventTypeThought && !emitThoughts {
			continue
		}
		select {
		case streamChan <- ev:
		case <-ctx.Done():
			trySendTerminal(streamChan, ev)
			drainBestEffort(streamChan, internalChan)
			return
		}
	}
}

// drainBestEffort pulls remaining frames off internalChan after ctx fires,
// forwarding only terminal frames so the consumer can still observe the
// loop's exit reason. Non-terminal frames are dropped to unblock
// runLogicLoop's send.
func drainBestEffort(streamChan chan<- StreamEvent, internalChan <-chan StreamEvent) {
	for ev := range internalChan {
		trySendTerminal(streamChan, ev)
	}
}

// regeneratedEvent builds the typed transition frame emitted at the start of
// a Regenerate stream. JSON-encoded into Content so wire consumers can parse
// without an Anthropic SDK dependency.
func regeneratedEvent(previousAssistantIndex, truncatedAt int) StreamEvent {
	return StreamEvent{
		Type:    EventTypeRegenerated,
		Content: fmt.Sprintf(`{"previous_assistant_index":%d,"truncated_at":%d}`, previousAssistantIndex, truncatedAt),
	}
}

// continuedEvent builds the typed transition frame emitted at the start of a
// Continue stream.
func continuedEvent(continuedFromIndex int) StreamEvent {
	return StreamEvent{
		Type:    EventTypeContinued,
		Content: fmt.Sprintf(`{"continued_from_index":%d}`, continuedFromIndex),
	}
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

// lastAssistantIndex returns the highest index of an "assistant" message,
// or -1 when none exists. Used to populate RegeneratedEvent.PreviousAssistantIndex
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
		// PatchDanglingToolCalls to seal it before the next LLM call.
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
