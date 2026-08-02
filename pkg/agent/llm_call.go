package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// callLLMWithRetry runs callLLM and retries transient errors per the
// configured RetryConfig. Retries are skipped once content has started
// streaming (consumer has already seen partial output). Returns the
// final content, result, and (possibly retry-exhausted) error.
func (al *AgentLoop) callLLMWithRetry(ctx context.Context, st *iterationState, msgs []history.Message) (string, LLMResult, error) {
	finalContent, result, _, err := al.callLLM(ctx, st, msgs)
	if err == nil || al.Retry == nil || !isRetryable(err) {
		return finalContent, result, err
	}
	for attempt := 0; attempt < al.Retry.MaxRetries; attempt++ {
		delay := al.Retry.delay(attempt)
		al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{
			Message: fmt.Sprintf("LLM error (%v). Retrying in %s (attempt %d/%d)...", err, delay, attempt+1, al.Retry.MaxRetries),
		}))
		if al.Retry.OnAttempt != nil {
			// 1-indexed: the user-visible "attempt 1/3" matches the loop's
			// retry semantics ("first retry"). Hook runs synchronously so
			// span events / metric increments stay associated with the loop
			// goroutine.
			al.Retry.OnAttempt(ctx, attempt+1, err, delay)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return finalContent, result, ctx.Err()
		}
		var retryContent string
		var contentEmitted bool
		retryContent, result, contentEmitted, err = al.callLLM(ctx, st, msgs)
		if retryContent != "" {
			finalContent = retryContent
		}
		if err == nil || contentEmitted || !isRetryable(err) {
			break
		}
	}
	return finalContent, result, err
}

// handleFinalAnswer appends the assistant's final-answer message,
// runs the optional Reflect rounds, persists session history, and emits
// Done. Save precedes Done so adopters who read history on Done (auto-
// titling, chat-list refresh) observe the assistant message rather than
// racing the persistence write. Caller must return immediately after this.
func (al *AgentLoop) handleFinalAnswer(ctx context.Context, st *iterationState, msgs []history.Message, finalContent string) {
	msgs = append(msgs, history.Message{Role: "assistant", Content: finalContent})
	if al.Reflect > 0 && finalContent != "" {
		msgs[len(msgs)-1].Content = al.runReflectRounds(ctx, st, msgs, finalContent)
	}
	al.saveSession(ctx, st.sessionKey, msgs)
	// Degraded precedes Done rather than replacing it: the answer is real
	// and consumers that gate on Done must still see it. No-op unless a
	// tool reported a partial success this Run.
	al.emitDegradedIfAny(ctx, st.sessionKey, st.streamChan)
	al.emit(ctx, st.sessionKey, st.streamChan, Event(DoneEvent{}))
}

// emitRunCostIfConfigured emits a RunCostEvent when a PriceTable is
// configured AND the Run actually accumulated tokens. Called from the
// deferred cleanup installed by installRunCostAccumulator so it fires
// on every terminal path — final answer, MaxIters cap, fatal LLM
// error — not just the success path. Skipped when the accumulator
// is missing (PriceTable nil) or the total is zero.
func (al *AgentLoop) emitRunCostIfConfigured(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent) {
	acc := runCostAccFromContext(ctx)
	if acc == nil {
		return
	}
	usage := acc.snapshot()
	if usage.TotalTokens == 0 && usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		return
	}
	al.emit(ctx, sessionKey, streamChan, Event(RunCostEvent{
		Model: al.PriceModel,
		Usage: usage,
		USD:   al.PriceTable.Compute(al.PriceModel, usage),
	}))
}

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
		al.emit(ctx, st.sessionKey, st.streamChan, Event(UsageEvent{Usage: res.Usage}))
		if acc := runCostAccFromContext(ctx); acc != nil {
			acc.add(res.Usage)
		}
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
			al.emit(ctx, sessionKey, streamChan, Event(ThoughtEvent{
				Message: fmt.Sprintf("Tool selector error, falling back to full registry: %v", selErr),
			}))
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
	msgsForLLM := withSoftLandingHint(iteration, al.MaxIters, msgs)
	msgsForLLM = al.withMemoryNotesInSystem(ctx, msgsForLLM)
	msgsForLLM = al.withToolChainingHint(msgsForLLM)
	msgsForLLM = al.withPlanModeHint(sessionKey, msgsForLLM)
	msgsForLLM = al.withDynamicContext(ctx, sessionKey, msgsForLLM)
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
