package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// callLLM runs one LLM stream attempt. Returns the accumulated content,
// the structured LLMResult, whether any content event was emitted (used
// to gate retry safety), and the call error.
//
// Resets the speculation map at the start of each attempt so a retry
// never sees orphans from the prior attempt; cancels their ctxs so
// well-behaved tools abort immediately.
func (al *AgentLoop) callLLM(ctx context.Context, st *iterationState, msgs []history.Message) (string, LLMResult, bool, error) {
	st.specMu.Lock()
	for k, sm := range st.specMap {
		sm.cancel()
		delete(st.specMap, k)
	}
	st.specMu.Unlock()

	for _, h := range al.BeforeLLMHooks {
		if h == nil {
			continue
		}
		if err := h(ctx, st.sessionKey, estimateTokens(msgs)); err != nil {
			return "", LLMResult{}, false, err
		}
	}

	pChan := make(chan StreamEvent, llmProviderBuffer)
	drainer := newStreamDrainer(al, st, ctx)
	done := make(chan struct{})
	go drainer.drain(pChan, done)

	toolsForCall := al.selectToolsForCall(ctx, st.sessionKey, st.streamChan, msgs)
	msgsForLLM := al.buildMsgsForLLM(ctx, st.sessionKey, st.iteration, msgs)

	llmCtx := ctx
	if al.ThinkingBudget > 0 {
		llmCtx = WithThinkingBudget(ctx, al.ThinkingBudget)
	}
	res, err := al.LLM.GenerateStream(llmCtx, msgsForLLM, toolsForCall, pChan)
	close(pChan)
	<-done

	content := drainer.content()
	if content == "" {
		content = res.Content
	}
	if err == nil && res.Usage.TotalTokens > 0 {
		payload, _ := json.Marshal(res.Usage)
		al.emit(ctx, st.sessionKey, st.streamChan, StreamEvent{Type: EventTypeUsage, Content: string(payload)})
	}
	return content, res, drainer.contentEmitted(), err
}

// selectToolsForCall picks the tool registry presented to the LLM for
// the current call: starts from al.Tools, adds the exit_plan_mode
// sentinel when plan mode is active, then optionally narrows via
// ToolSelector. Selector errors fall back to the full registry with a
// thought event for visibility.
func (al *AgentLoop) selectToolsForCall(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, msgs []history.Message) *tools.Registry {
	toolsForCall := al.Tools
	if al.IsPlanMode(sessionKey) {
		toolsForCall = withPlanModeTool(toolsForCall)
	}
	if al.ToolSelector != nil {
		query := latestUserMessage(msgs)
		if filtered, selErr := al.ToolSelector.SelectRegistry(ctx, query); selErr == nil && filtered != nil {
			toolsForCall = filtered
		} else if selErr != nil {
			al.emit(ctx, sessionKey, streamChan, StreamEvent{
				Type:    "thought",
				Content: fmt.Sprintf("Tool selector error, falling back to full registry: %v", selErr),
			})
		}
	}
	return toolsForCall
}

// buildMsgsForLLM produces the slice handed to LLM.GenerateStream by
// chaining the system-prompt augmentations (soft landing, tool chaining
// hint, plan mode hint, dynamic context) and stamping AutoCacheSystem
// when enabled. The returned slice is safe to pass to providers — the
// caller's msgs is never mutated even when the chain takes the alias
// fast path.
func (al *AgentLoop) buildMsgsForLLM(ctx context.Context, sessionKey string, iteration int, msgs []history.Message) []history.Message {
	msgsForLLM := al.withDynamicContext(ctx, sessionKey, al.withPlanModeHint(sessionKey, al.withToolChainingHint(withSoftLandingHint(iteration, al.MaxIters, msgs))))
	if al.AutoCacheSystem && len(msgsForLLM) > 0 && msgsForLLM[0].Role == "system" && !msgsForLLM[0].CacheHint {
		// Copy before stamping: msgsForLLM can alias the input slice
		// when no upstream hint needed a fresh allocation. Mutating the
		// alias would leak CacheHint into the caller's session-loaded
		// slice.
		stamped := make([]history.Message, len(msgsForLLM))
		copy(stamped, msgsForLLM)
		stamped[0].CacheHint = true
		msgsForLLM = stamped
	}
	return msgsForLLM
}
