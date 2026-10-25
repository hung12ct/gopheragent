package agent

import (
	"context"
	"fmt"
	"iter"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// Resume answers approvals that a DeferApprovals run left pending and
// continues that run. decisions maps a pending call's ID (the
// ActionRequiredEvent.ID, or PendingToolCall.ID from PendingApprovals) to
// approve (true) or deny (false).
//
// The state lives in session history, so Resume works from any process that
// shares the SessionManager, including after a restart. It finishes the
// session's latest tool turn: approved calls execute, denied ones get the
// usual denial result, calls that never ran because a later wave waited on
// an approval execute now, and gated calls without a decision ask again
// (deferred again under DeferApprovals). Once every call has a result the
// loop continues as a normal run; EventTypeContinued is the first frame.
//
// Returns ErrNothingToResume when no call is pending, and ErrUnknownApproval
// when decisions names an ID that PendingApprovals would not list, so a
// decision can never be silently ignored. Resume does not serialize
// callers: two concurrent Resumes for one session would both run an
// approved call, so route a session's answers through one goroutine.
func (al *AgentLoop) Resume(ctx context.Context, sessionKey string, decisions map[string]bool) (iter.Seq[StreamEvent], error) {
	existing, err := al.Sessions.History(ctx, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("agent: resume: load history: %w", err)
	}
	idx, pending := pendingToolTurn(existing)
	if len(pending) == 0 {
		return nil, ErrNothingToResume
	}
	open := al.approvalsIn(WithSessionKey(ctx, sessionKey), pending)
	for id := range decisions {
		if !containsCallID(open, id) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownApproval, id)
		}
	}

	firstFrame := continuedEvent(idx)
	emitThoughts := al.EmitThoughts
	return func(yield func(StreamEvent) bool) {
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if !yield(firstFrame) {
			return
		}
		internalChan := make(chan StreamEvent, runIterationStreamBuffer)
		go al.resumeLogicLoop(runCtx, sessionKey, decisions, internalChan)
		yieldEvents(internalChan, yield, emitThoughts, cancel)
	}, nil
}

// PendingApprovals lists the calls in sessionKey's latest tool turn that
// still need a human decision, one entry per distinct call (identical
// duplicates share the leader's decision). Use it after a restart to ask
// the user again, or to route an incoming reply to Resume instead of Run.
func (al *AgentLoop) PendingApprovals(ctx context.Context, sessionKey string) ([]PendingToolCall, error) {
	existing, err := al.Sessions.History(ctx, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("agent: pending approvals: load history: %w", err)
	}
	_, pending := pendingToolTurn(existing)
	return al.approvalsIn(WithSessionKey(ctx, sessionKey), pending), nil
}

// approvalsIn filters pending down to the calls that wait on a decision:
// duplicates folded into their leader, and calls the gate would not stop
// (ungated calls from a later wave) left out.
func (al *AgentLoop) approvalsIn(ctx context.Context, pending []PendingToolCall) []PendingToolCall {
	leaders, _ := al.splitGatedDuplicates(pending)
	var out []PendingToolCall
	for _, tc := range leaders {
		if al.needsApproval(ctx, tc) {
			out = append(out, tc)
		}
	}
	return out
}

// resumeLogicLoop finishes the pending tool turn with decisions applied,
// then hands over to iterateMessages. Mirrors continueLogicLoop's setup.
func (al *AgentLoop) resumeLogicLoop(ctx context.Context, sessionKey string, decisions map[string]bool, streamChan chan<- StreamEvent) {
	defer close(streamChan)
	ctx = WithSessionKey(ctx, sessionKey)

	var emitCost func()
	ctx, emitCost = al.installRunCostAccumulator(ctx, sessionKey, streamChan)
	defer emitCost()
	var sweepDegraded func()
	ctx, sweepDegraded = al.installDegradationAccumulator(ctx, sessionKey, streamChan)
	defer sweepDegraded()

	msgs, err := al.Sessions.History(ctx, sessionKey)
	if err != nil {
		al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("agent: resume: load history: %w", err)))
		return
	}
	// History may have moved between Resume and this goroutine.
	idx, pending := pendingToolTurn(msgs)
	if len(pending) == 0 {
		al.emit(ctx, sessionKey, streamChan, errEvent(ErrNothingToResume))
		return
	}

	st := &iterationState{
		sessionKey: sessionKey,
		streamChan: streamChan,
		specMap:    newSpeculativeMap(),
		specMu:     &sync.Mutex{},
		tracker:    loopDetectorFromHistory(msgs),
		decisions:  decisions,
	}
	calls := pendingCallsOf(msgs[idx])
	ws := seededWaveState(msgs[idx+1:], len(calls))
	al.executeToolWaves(ctx, st, ws, calls)

	// Rebuild the tool block in call order: answers recorded before the
	// suspension and those produced now interleave by call position.
	turn := append(make([]history.Message, 0, idx+1+len(calls)), msgs[:idx+1]...)
	if done, _ := al.recordToolTurn(ctx, st, &turn, calls, nil, ws); done {
		return
	}
	al.iterateMessages(ctx, sessionKey, streamChan, turn)
	al.fireConsolidator(ctx, sessionKey)
}

// pendingToolTurn locates the latest tool turn: the last message must be an
// assistant message with tool calls, or a tool result inside the block that
// follows one. It returns that assistant message's index and its calls that
// have no result yet; idx is -1 and pending nil when there is no such turn.
func pendingToolTurn(msgs []history.Message) (int, []PendingToolCall) {
	i := len(msgs) - 1
	for i >= 0 && msgs[i].Role == "tool" {
		i--
	}
	if i < 0 || msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
		return -1, nil
	}
	answered := make(map[string]struct{}, len(msgs)-1-i)
	for _, m := range msgs[i+1:] {
		answered[m.ToolCallID] = struct{}{}
	}
	var pending []PendingToolCall
	for _, tc := range pendingCallsOf(msgs[i]) {
		if _, ok := answered[tc.ID]; !ok {
			pending = append(pending, tc)
		}
	}
	if len(pending) == 0 {
		return -1, nil
	}
	return i, pending
}

// pendingCallsOf converts an assistant message's tool calls back into the
// loop's dispatch shape.
func pendingCallsOf(m history.Message) []PendingToolCall {
	calls := make([]PendingToolCall, len(m.ToolCalls))
	for i, tc := range m.ToolCalls {
		calls[i] = PendingToolCall{ID: tc.ID, Name: tc.Name, ArgsJSON: tc.Arguments}
	}
	return calls
}

// seededWaveState returns a waveState holding the tool results already in
// history, so executeToolWaves skips those calls and <output_of:...>
// references to them still resolve.
func seededWaveState(results []history.Message, capacity int) *waveState {
	ws := newWaveState(capacity)
	for _, m := range results {
		ws.toolMsgs[m.ToolCallID] = m
		if !m.IsError {
			ws.resultsByID[m.ToolCallID] = m.Content
		}
	}
	return ws
}

// needsApproval reports whether tc would stop at the HITL gate, applying
// the same permission rules as executeToolCall.
func (al *AgentLoop) needsApproval(ctx context.Context, tc PendingToolCall) bool {
	tool, ok := al.Tools.Get(tc.Name)
	if !ok {
		return false
	}
	decision := PermissionPrompt
	if al.Permissions != nil {
		decision = al.Permissions.Check(ctx, tc.Name, tc.ArgsJSON)
	}
	if decision == PermissionAllow || decision == PermissionDeny {
		return false
	}
	return tool.Descriptor().RequiresConfirmation || decision == PermissionConfirm
}

func containsCallID(calls []PendingToolCall, id string) bool {
	for _, tc := range calls {
		if tc.ID == id {
			return true
		}
	}
	return false
}
