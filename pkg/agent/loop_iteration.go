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

	finalContent, result, err := al.callLLMWithRetry(ctx, st, msgsForLLM)
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

	ws := al.executeToolWaves(ctx, st, scheduled)
	if ws.fatalErr != nil {
		iterErr = ws.fatalErr
		al.saveSession(ctx, sessionKey, *msgs)
		al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("%w: %w", ErrLoopDetected, ws.fatalErr)))
		return len(scheduled), true
	}

	synthesizeDroppedToolErrors(ws.toolMsgs, droppedCalls, al.MaxToolCallsPerTurn)
	*msgs = appendToolResultsInOrder(*msgs, result.ToolCalls, ws.toolMsgs)
	if err := al.Sessions.SaveHistory(ctx, sessionKey, *msgs); err != nil {
		al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("agent: save history: %w", err)))
	}
	return len(scheduled), false
}
