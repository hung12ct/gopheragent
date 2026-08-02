package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// newToolCallID returns an opaque hex correlation ID for one tool dispatch.
// Distinct from PendingToolCall.ID — providers like Gemini reuse tool names
// as their call ID, which collides for parallel calls of the same tool.
func newToolCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// executeToolWaves runs the scheduled tool calls in dependency waves
// using a fresh waveState. Substitution of <output_of:...> references
// happens just-in-time at the start of each wave; failed substitutions
// short-circuit with a synthesized tool-error result. A fatal error
// recorded by any goroutine (e.g. anti-loop detector) breaks out of
// the wave loop.
func (al *AgentLoop) executeToolWaves(ctx context.Context, st *iterationState, scheduled []PendingToolCall) *waveState {
	waves, schedErr := scheduleToolCalls(scheduled)
	if schedErr != nil {
		al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{
			Message: fmt.Sprintf("Tool scheduler: %v — running all calls in one wave.", schedErr),
		}))
		waves = [][]PendingToolCall{scheduled}
	}

	ws := newWaveState(len(scheduled))
	for _, wave := range waves {
		substitutedWave := substituteWaveArgs(ws, wave, func(toolName string, subErr error) {
			al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{
				Message: fmt.Sprintf("Tool scheduler: substitution for %q failed: %v — skipping execution.", toolName, subErr),
			}))
		})

		var wg sync.WaitGroup
		for _, tc := range substitutedWave {
			wg.Add(1)
			go func(tCall PendingToolCall) {
				defer wg.Done()
				al.executeToolCall(ctx, st, ws, tCall)
			}(tc)
		}
		wg.Wait()

		if ws.fatalErr != nil {
			break
		}
	}
	return ws
}

// executeToolCall is the per-tool goroutine body. The caller spawns it
// with `go al.executeToolCall(...)`; every return path matches the
// original inline goroutine 1:1 — same lock acquisitions, same write
// targets, same emit ordering. Refactor cardinal rule: do not change
// behavior here.
func (al *AgentLoop) executeToolCall(ctx context.Context, st *iterationState, ws *waveState, tCall PendingToolCall) {
	ws.fatalMu.Lock()
	hasFatal := ws.fatalErr != nil
	ws.fatalMu.Unlock()
	if hasFatal {
		return
	}

	// Plan-mode gate runs before the tool-registry lookup so
	// exit_plan_mode can be a loop-level sentinel even when the caller
	// did not register a concrete tool for it.
	if al.runPlanModeGate(ctx, st.sessionKey, st.streamChan, ws, tCall) {
		return
	}

	tool, ok := al.Tools.Get(tCall.Name)
	if !ok {
		toolErr := &ToolNotFoundError{ToolName: tCall.Name}
		al.emit(ctx, st.sessionKey, st.streamChan, errEvent(toolErr))
		ws.recordToolMsg(tCall.ID, history.Message{
			Role:       "tool",
			Content:    toolErr.Error(),
			ToolCallID: tCall.ID,
			IsError:    true,
		}, false)
		return
	}
	desc := tool.Descriptor()

	// Consult the permission policy before HITL. Allow bypasses
	// RequiresConfirmation; Deny short-circuits even for tools that
	// would otherwise run without a prompt; Prompt falls through to the
	// existing HITL flow.
	permDecision := PermissionPrompt
	if al.Permissions != nil {
		permDecision = al.Permissions.Check(ctx, tCall.Name, tCall.ArgsJSON)
	}
	if permDecision == PermissionAllow && desc.RequiresConfirmation {
		al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{Message: fmt.Sprintf("Permission policy auto-approved %s (bypassing HITL).", tCall.Name)}))
	}
	if permDecision == PermissionDeny {
		deniedErr := &PermissionDeniedError{ToolName: tCall.Name}
		al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{Message: fmt.Sprintf("Permission policy denied %s — skipping execution.", tCall.Name)}))
		ws.recordToolMsg(tCall.ID, history.Message{
			Role:       "tool",
			Content:    fmt.Sprintf("%v. This is a permission gate, not a tool failure — the policy denied the call deterministically; do not retry. Tell the user the requested action requires a permission they have not granted, and ask whether they have an alternative approach.", deniedErr),
			ToolCallID: tCall.ID,
			IsError:    true,
		}, false)
		return
	}

	// PermissionConfirm escalates a tool that would otherwise run ungated;
	// RequiresConfirmation covers tools that always prompt. Allow bypasses
	// both.
	if (desc.RequiresConfirmation || permDecision == PermissionConfirm) && permDecision != PermissionAllow {
		outcome := al.runHITLGate(ctx, st.sessionKey, st.streamChan, ws, tCall)
		if outcome != hitlApproved {
			var msg string
			switch outcome {
			case hitlUnconfigured:
				gateErr := &ConfirmationGateUnconfiguredError{ToolName: tCall.Name}
				msg = fmt.Sprintf("%v. This is a host-side configuration bug, not a user denial — do not tell the user they refused. Tell them the tool needs operator approval that has not been wired up, and ask whether you should proceed with an alternative the tool gate does not cover.", gateErr)
			case hitlTimedOut:
				timeoutErr := &HITLTimedOutError{ToolName: tCall.Name, Timeout: al.ConfirmHITLTimeout}
				msg = fmt.Sprintf("%v. The user did NOT refuse — the approval prompt expired before they could respond. Do not paraphrase this as a denial or seek a workaround. Tell the user the approval window closed and ask them to repeat the request when they are ready to confirm.", timeoutErr)
			default:
				deniedErr := &HITLDeniedError{ToolName: tCall.Name}
				msg = fmt.Sprintf("%v. This is a permission gate, not a tool failure. If the user's task genuinely needs this tool, ask them explicitly to grant permission rather than silently using a workaround.", deniedErr)
			}
			ws.recordToolMsg(tCall.ID, history.Message{
				Role:       "tool",
				Content:    msg,
				ToolCallID: tCall.ID,
				IsError:    true,
			}, false)
			return
		}
		al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{Message: "Human APPROVED tool execution."}))
	}

	var cacheKey string
	cacheOK := false
	if al.Cache != nil && desc.Cacheable {
		cacheOK = true
		cacheKey = toolCacheKey(tCall.Name, tCall.ArgsJSON)
	}

	// Peek the speculative map before emitting so the tool_call event can
	// flag Reused=true at announcement time. The actual await/execute path
	// below uses the same (sm, speculated) values.
	st.specMu.Lock()
	sm, speculated := st.specMap[tCall.ID]
	st.specMu.Unlock()

	callID := newToolCallID()
	al.emit(ctx, st.sessionKey, st.streamChan, Event(ToolCallEvent{
		ID:       callID,
		Name:     tCall.Name,
		ArgsJSON: tCall.ArgsJSON,
		Reused:   speculated,
	}))

	if cacheOK {
		if cached, hit := al.Cache.Get(cacheKey); hit {
			al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{Message: fmt.Sprintf("Cache hit for %s, skipping execution.", tCall.Name)}))
			ws.recordToolMsg(tCall.ID, history.Message{Role: "tool", Content: cached, ToolCallID: tCall.ID, CorrelationID: callID}, true)
			return
		}
	}

	toolCtx := tools.WithProgressFunc(ctx, func(msg string) {
		ev := Event(ToolProgressEvent{
			Name:       tCall.Name,
			ToolCallID: callID,
			Message:    msg,
		})
		select {
		case st.streamChan <- ev:
			for _, h := range al.EventHandlers {
				safeCallHandler(h, ctx, st.sessionKey, ev)
			}
		default:
		}
	})
	toolCtx = WithDynamicContextFunc(toolCtx, al.DynamicContext)
	toolCtx = WithConfirmHITL(toolCtx, al.ConfirmHITL)
	toolCtx = WithConfirmHITLTimeout(toolCtx, al.ConfirmHITLTimeout)
	toolCtx = tools.WithToolCallID(toolCtx, callID)
	toolCtx = WithSubAgentEmitter(toolCtx, func(ev StreamEvent) {
		select {
		case st.streamChan <- ev:
			for _, h := range al.EventHandlers {
				safeCallHandler(h, ctx, st.sessionKey, ev)
			}
		default:
		}
	})
	// If the drainer speculatively started this call mid-stream, block
	// on its result rather than re-executing. The speculation is always
	// for the exact argsJSON we have now because shouldSpeculate refuses
	// to speculate anything that could later be rewritten.
	var toolResult string
	var structured any
	var execErr error
	var degraded *tools.Degradation
	if speculated {
		al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{Message: fmt.Sprintf("Reusing speculative result for %s.", tCall.Name)}))
		toolResult, structured, degraded, execErr = awaitSpeculative(toolCtx, sm)
	} else {
		res, err := tool.Execute(toolCtx, tCall.ArgsJSON)
		toolResult, structured, degraded, execErr = res.Text, res.Structured, res.Degraded, err
	}
	if al.OnToolResult != nil {
		rewritten, hookErr := al.OnToolResult(toolCtx, callID, tCall.Name, tCall.ArgsJSON, toolResult, structured, execErr)
		// Hook semantics:
		//   success in → (rewritten, nil): result rewritten
		//   success in → (_, hookErr):     conversion to tool error
		//   error in   → (_, nil):         hook recovered the call (rewritten becomes result)
		//   error in   → (_, hookErr):     hook replaced the original error
		if hookErr != nil {
			execErr = hookErr
		} else {
			toolResult = rewritten
			execErr = nil
		}
	}
	toolResult = applyDegradation(ctx, tCall.Name, toolResult, degraded, execErr, speculated)

	content := toolResult
	isToolErr := execErr != nil
	if isToolErr {
		content = al.formatToolError(ToolErrorContext{
			ToolName:  tCall.Name,
			ArgsJSON:  tCall.ArgsJSON,
			Iteration: st.iteration,
			Cause:     execErr,
		})
	}

	isInlineResult := false
	if !isToolErr && desc.Inline {
		isInlineResult = true
		al.emit(ctx, st.sessionKey, st.streamChan, Event(ContentEvent{Text: "\n\n" + content + "\n\n"}))
	}

	// Never cache a degraded result. Its text carries a partial-success
	// note naming artifacts that landed *this* run, and a cache hit
	// short-circuits before recordDegradation — so replaying it would
	// tell a later turn's model that work already exists while the host
	// sees a clean turn with no DegradedEvent.
	if cacheOK && !isToolErr && degraded == nil {
		al.Cache.Put(cacheKey, content)
	}

	st.tracker.AddCall(tCall.Name, tCall.ArgsJSON, content)
	warnMessage, loopErr := st.tracker.Detect()
	if loopErr != nil {
		ws.setFatal(loopErr)
		return
	}
	if warnMessage != "" {
		content += "\n\n" + warnMessage
		al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{Message: "System inserted an anti-loop warning into context window."}))
	}

	ws.recordToolMsg(tCall.ID, history.Message{
		Role:           "tool",
		Content:        content,
		ToolCallID:     tCall.ID,
		CorrelationID:  callID,
		IsError:        isToolErr,
		IsInlineResult: isInlineResult,
	}, !isToolErr)
}
