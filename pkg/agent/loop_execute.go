package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// executeToolWaves runs the scheduled tool calls in dependency waves
// using a fresh waveState. Substitution of <output_of:...> references
// happens just-in-time at the start of each wave; failed substitutions
// short-circuit with a synthesized tool-error result. A fatal error
// recorded by any goroutine (e.g. anti-loop detector) breaks out of
// the wave loop.
func (al *AgentLoop) executeToolWaves(ctx context.Context, st *iterationState, scheduled []PendingToolCall) *waveState {
	waves, schedErr := ScheduleToolCalls(scheduled)
	if schedErr != nil {
		al.emit(ctx, st.sessionKey, st.streamChan, StreamEvent{
			Type:    "thought",
			Content: fmt.Sprintf("Tool scheduler: %v — running all calls in one wave.", schedErr),
		})
		waves = [][]PendingToolCall{scheduled}
	}

	ws := newWaveState(len(scheduled))
	for _, wave := range waves {
		substitutedWave := substituteWaveArgs(ws, wave, func(toolName string, subErr error) {
			al.emit(ctx, st.sessionKey, st.streamChan, StreamEvent{
				Type:    "thought",
				Content: fmt.Sprintf("Tool scheduler: substitution for %q failed: %v — skipping execution.", toolName, subErr),
			})
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

	// Consult the permission policy before HITL. Allow bypasses
	// RequiresConfirmation(); Deny short-circuits even for tools that
	// would otherwise run without a prompt; Prompt falls through to the
	// existing HITL flow.
	permDecision := PermissionPrompt
	if al.Permissions != nil {
		permDecision = al.Permissions.Check(ctx, tCall.Name, tCall.ArgsJSON)
	}
	if permDecision == PermissionAllow && tool.RequiresConfirmation() {
		al.emit(ctx, st.sessionKey, st.streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf("Permission policy auto-approved %s (bypassing HITL).", tCall.Name)})
	}
	if permDecision == PermissionDeny {
		deniedErr := &PermissionDeniedError{ToolName: tCall.Name}
		al.emit(ctx, st.sessionKey, st.streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf("Permission policy denied %s — skipping execution.", tCall.Name)})
		ws.recordToolMsg(tCall.ID, history.Message{
			Role:       "tool",
			Content:    fmt.Sprintf("%v. This is a permission gate, not a tool failure — the policy denied the call deterministically; do not retry. Tell the user the requested action requires a permission they have not granted, and ask whether they have an alternative approach.", deniedErr),
			ToolCallID: tCall.ID,
			IsError:    true,
		}, false)
		return
	}

	if tool.RequiresConfirmation() && permDecision != PermissionAllow {
		approved := al.runHITLGate(ctx, st.sessionKey, st.streamChan, ws, tCall)
		if !approved {
			deniedErr := &HITLDeniedError{ToolName: tCall.Name}
			ws.recordToolMsg(tCall.ID, history.Message{
				Role:       "tool",
				Content:    fmt.Sprintf("%v. This is a permission gate, not a tool failure. If the user's task genuinely needs this tool, ask them explicitly to grant permission rather than silently using a workaround.", deniedErr),
				ToolCallID: tCall.ID,
				IsError:    true,
			}, false)
			return
		}
		al.emit(ctx, st.sessionKey, st.streamChan, StreamEvent{Type: EventTypeThought, Content: "Human APPROVED tool execution."})
	}

	cacheKey := toolCacheKey(tCall.Name, tCall.ArgsJSON)
	cacheOK := false
	if al.Cache != nil {
		if c, ok := tool.(tools.Cacheable); ok && c.Cacheable() {
			cacheOK = true
		}
	}

	al.emit(ctx, st.sessionKey, st.streamChan, StreamEvent{Type: EventTypeToolCall, Name: tCall.Name, Content: fmt.Sprintf("Executing: %s", tCall.Name)})

	if cacheOK {
		if cached, hit := al.Cache.Get(cacheKey); hit {
			al.emit(ctx, st.sessionKey, st.streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf("Cache hit for %s, skipping execution.", tCall.Name)})
			ws.recordToolMsg(tCall.ID, history.Message{Role: "tool", Content: cached, ToolCallID: tCall.ID}, true)
			return
		}
	}

	toolCtx := tools.WithProgressFunc(ctx, func(msg string) {
		ev := StreamEvent{Type: EventTypeToolProgress, Content: msg}
		select {
		case st.streamChan <- ev:
			for _, h := range al.EventHandlers {
				safeCallHandler(h, ctx, st.sessionKey, ev)
			}
		default:
		}
	})
	toolCtx = WithDynamicContextFunc(toolCtx, al.DynamicContext)
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
	st.specMu.Lock()
	sm, speculated := st.specMap[tCall.ID]
	st.specMu.Unlock()
	var toolResult string
	var structured any
	var execErr error
	if speculated {
		al.emit(ctx, st.sessionKey, st.streamChan, StreamEvent{Type: EventTypeThought, Content: fmt.Sprintf("Reusing speculative result for %s.", tCall.Name)})
		toolResult, execErr = awaitSpeculative(toolCtx, sm)
	} else if sr, ok := tool.(tools.StructuredResult); ok {
		// Tool advertises a typed payload — prefer the structured path so
		// the payload reaches OnToolResult. The string branch (Execute)
		// stays the fallback for tools that don't opt in.
		toolResult, structured, execErr = sr.ExecuteStructured(toolCtx, tCall.ArgsJSON)
	} else {
		toolResult, execErr = tool.Execute(toolCtx, tCall.ArgsJSON)
	}
	if execErr == nil && al.OnToolResult != nil {
		rewritten, hookErr := al.OnToolResult(toolCtx, tCall.Name, tCall.ArgsJSON, toolResult, structured)
		if hookErr != nil {
			execErr = hookErr
		} else {
			toolResult = rewritten
		}
	}
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
	if !isToolErr {
		if ir, ok := tool.(tools.InlineRenderer); ok && ir.InlineResult() {
			isInlineResult = true
			al.emit(ctx, st.sessionKey, st.streamChan, StreamEvent{Type: EventTypeContent, Content: "\n\n" + content + "\n\n"})
		}
	}

	if cacheOK && !isToolErr {
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
		al.emit(ctx, st.sessionKey, st.streamChan, StreamEvent{Type: EventTypeThought, Content: "System inserted an anti-loop warning into context window."})
	}

	ws.recordToolMsg(tCall.ID, history.Message{
		Role:           "tool",
		Content:        content,
		ToolCallID:     tCall.ID,
		IsError:        isToolErr,
		IsInlineResult: isInlineResult,
	}, !isToolErr)
}
