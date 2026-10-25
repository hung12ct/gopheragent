package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// runIteration executes one iteration of the agent loop. Returns
// scheduledToolCalls (the number of tool calls actually dispatched to
// wave execution this iteration — used by the caller to enforce
// MaxToolCallsPerSession) and done=true when the loop must terminate
// (final answer reached, fatal error encountered, or ctx cancelled).
// Mutates *msgs in place — caller observes the new slice header on return.
//
// Per-iteration shape:
//  1. ctx-cancel check
//  2. token-budget enforcement + soft-landing nudge
//  3. callLLM (with retry on transient errors)
//  4. terminal branch when no tool calls (handleFinalAnswer)
//  5. tool-call budget split + assistant msg append
//  6. tool-wave execution
//  7. fatal-error gate / dropped-call synthesis / tool-result drain
func (al *AgentLoop) runIteration(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, msgs *[]history.Message, iteration int, tracker *loopDetector) (int, bool) {
	if ctx.Err() != nil {
		// Persist before signaling so adopters that read history on the
		// terminal event observe the durable state, not a partial in-memory
		// snapshot. See handleFinalAnswer for the canonical sequencing.
		al.saveSession(ctx, sessionKey, *msgs)
		al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("%w: %w", ErrContextCancelled, ctx.Err())))
		return 0, true
	}

	// Open the per-iteration span; the reassigned ctx carries it into the LLM
	// call and tool waves so their spans nest as children. No-op when telemetry
	// is disabled (see startIterationSpan).
	ctx, endSpan := al.startIterationSpan(ctx, sessionKey, iteration)
	var iterErr error
	defer func() { endSpan(iterErr) }()

	// Pruning is transient: the trimmed slice fuels just this LLM call.
	// *msgs stays at full fidelity so SetHistory / saveSession persist the
	// untouched conversation — without this distinction every prune would
	// shrink the on-disk history forever.
	msgsForLLM := al.enforceTokenBudget(ctx, sessionKey, streamChan, iteration, *msgs)
	al.emitSoftLandingNudge(ctx, sessionKey, streamChan, iteration)

	specMap := newSpeculativeMap()
	var specMu sync.Mutex
	st := &iterationState{
		sessionKey: sessionKey,
		iteration:  iteration,
		streamChan: streamChan,
		specMap:    specMap,
		specMu:     &specMu,
		tracker:    tracker,
	}

	snap := al.snapshotRequest(*msgs)
	finalContent, result, err := al.callLLMWithRetry(ctx, st, msgsForLLM)
	al.checkRequestInvariant(ctx, snap, *msgs, st.sentMessages)
	if err != nil {
		al.saveSession(ctx, sessionKey, *msgs)
		// A cancel that lands while the provider stream is in flight surfaces
		// as a provider error. Classify it as cancellation (not an LLM
		// failure) so adopters' errors.Is(err, ErrContextCancelled) checks
		// fire — mirroring the pre-call cancel path above.
		if cause := ctx.Err(); cause != nil || errors.Is(err, context.Canceled) {
			if cause == nil {
				cause = err
			}
			// Mark the iteration span as cancelled so the WithTracer path agrees
			// with the ErrorEvent stream (which classifies this as cancellation).
			iterErr = fmt.Errorf("%w: %w", ErrContextCancelled, cause)
			al.emit(ctx, sessionKey, streamChan, errEvent(iterErr))
			return 0, true
		}
		iterErr = err
		al.emit(ctx, sessionKey, streamChan, errEvent(&LLMFailureError{Cause: err}))
		return 0, true
	}

	scheduled, droppedCalls := al.applyToolCallBudget(ctx, sessionKey, streamChan, result.ToolCalls)

	if len(result.ToolCalls) == 0 {
		al.handleFinalAnswer(ctx, st, *msgs, finalContent)
		return 0, true
	}

	*msgs = appendAssistantToolCallMsg(*msgs, finalContent, result.ToolCalls)
	if err := al.Sessions.SaveHistory(ctx, sessionKey, *msgs); err != nil {
		al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("agent: save history: %w", err)))
	}

	ws := newWaveState(len(scheduled))
	al.executeToolWaves(ctx, st, ws, scheduled)
	done, iterErr := al.recordToolTurn(ctx, st, msgs, result.ToolCalls, droppedCalls, ws)
	return len(scheduled), done
}

// recordToolTurn appends a finished tool wave's results to *msgs in call
// order, persists them, and reports whether the run must end. It ends on a
// fatal wave error (every unanswered call is sealed so the transcript stays
// valid) and on deferred approvals, whose calls are left without a result
// for Resume to answer. The returned error is for the iteration span; the
// terminal event has already been emitted.
func (al *AgentLoop) recordToolTurn(ctx context.Context, st *iterationState, msgs *[]history.Message, calls, dropped []PendingToolCall, ws *waveState) (bool, error) {
	if ws.fatalErr != nil {
		// The assistant message persisted above carries tool_calls, so the
		// transcript is only valid once every call has a tool reply. Keep the
		// results that completed before the abort (real work, and the looping
		// call's own result is what the detector tripped on) and account for
		// the rest, so the saved history needs no downstream repair.
		synthesizeMissingToolErrors(ws.toolMsgs, calls, fatalAbortToolReason)
		*msgs = appendToolResultsInOrder(*msgs, calls, ws.toolMsgs)
		al.saveSession(ctx, st.sessionKey, *msgs)
		al.emit(ctx, st.sessionKey, st.streamChan, errEvent(fmt.Errorf("%w: %w", ErrLoopDetected, ws.fatalErr)))
		return true, ws.fatalErr
	}

	synthesizeDroppedToolErrors(ws.toolMsgs, dropped, al.MaxToolCallsPerTurn)
	*msgs = appendToolResultsInOrder(*msgs, calls, ws.toolMsgs)
	if len(ws.pending) > 0 {
		// Persist even when ctx was cancelled: the saved turn is what Resume
		// reads, possibly from another process.
		al.saveSession(ctx, st.sessionKey, *msgs)
		al.emit(ctx, st.sessionKey, st.streamChan, errEvent(fmt.Errorf("%w: %d call(s)", ErrApprovalPending, len(ws.pending))))
		return true, nil
	}
	if err := al.Sessions.SaveHistory(ctx, st.sessionKey, *msgs); err != nil {
		al.emit(ctx, st.sessionKey, st.streamChan, errEvent(fmt.Errorf("agent: save history: %w", err)))
	}
	return false, nil
}
