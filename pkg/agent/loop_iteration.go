package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// runIteration executes one iteration of the agent loop. Returns
// done=true when the loop must terminate (final answer reached, fatal
// error encountered, or ctx cancelled). Mutates *msgs in place — caller
// observes the new slice header on return.
//
// Per-iteration shape:
//  1. ctx-cancel check
//  2. token-budget enforcement + soft-landing nudge
//  3. callLLM (with retry on transient errors)
//  4. terminal branch when no tool calls (handleFinalAnswer)
//  5. tool-call budget split + assistant msg append
//  6. tool-wave execution
//  7. fatal-error gate / dropped-call synthesis / tool-result drain
func (al *AgentLoop) runIteration(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, msgs *[]history.Message, iteration int, tracker *LoopDetector) bool {
	if ctx.Err() != nil {
		al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("%w: %w", ErrContextCancelled, ctx.Err())))
		al.saveSession(ctx, sessionKey, *msgs)
		return true
	}

	*msgs = al.enforceTokenBudget(ctx, sessionKey, streamChan, *msgs)
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

	finalContent, result, err := al.callLLMWithRetry(ctx, st, *msgs)
	if err != nil {
		al.emit(ctx, sessionKey, streamChan, errEvent(&LLMFailureError{Cause: err}))
		al.saveSession(ctx, sessionKey, *msgs)
		return true
	}

	scheduled, droppedCalls := al.applyToolCallBudget(ctx, sessionKey, streamChan, result.ToolCalls)

	if len(result.ToolCalls) == 0 {
		al.handleFinalAnswer(ctx, st, *msgs, finalContent)
		return true
	}

	*msgs = appendAssistantToolCallMsg(*msgs, finalContent, result.ToolCalls)
	al.Sessions.SetHistory(ctx, sessionKey, *msgs)

	ws := al.executeToolWaves(ctx, st, scheduled)
	if ws.fatalErr != nil {
		al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("%w: %w", ErrLoopDetected, ws.fatalErr)))
		al.saveSession(ctx, sessionKey, *msgs)
		return true
	}

	synthesizeDroppedToolErrors(ws.toolMsgs, droppedCalls, al.MaxToolCallsPerTurn)
	*msgs = appendToolResultsInOrder(*msgs, result.ToolCalls, ws.toolMsgs)
	al.Sessions.SetHistory(ctx, sessionKey, *msgs)
	return false
}
