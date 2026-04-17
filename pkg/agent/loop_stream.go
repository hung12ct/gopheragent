// Package agent provides the core ReAct loop, streaming, and orchestration for LLM agents.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/hung12ct/gopheragent/pkg/cache"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// Hook defines a middleware function that runs before the agent loop.
type Hook func(ctx context.Context, sessionKey string, userInput string) error

// ConfirmFunc is called when a tool declares RequiresConfirmation().
// It blocks the loop until a human decision is made.
// Return true to approve execution, false to deny.
// The toolName and argsJSON are provided for display to the user.
type ConfirmFunc func(ctx context.Context, toolName string, argsJSON string) bool

// EventHandler is called for every StreamEvent emitted during the agent loop,
// including events from LLM providers. It runs synchronously in the loop goroutine.
// Use for metrics, structured logging, token tracking, or custom analytics.
// sessionKey identifies which conversation the event belongs to.
type EventHandler func(ctx context.Context, sessionKey string, ev StreamEvent)

// SessionManager interface abstracts history storage.
//
// Breaking-change note: DeleteSession and Fork were added for ephemeral worker
// sessions and conversation branching respectively. Any external implementation
// of this interface must provide them.
type SessionManager interface {
	GetHistory(ctx context.Context, sessionKey string) []history.Message
	SetHistory(ctx context.Context, sessionKey string, messages []history.Message)
	GetAsyncTasks(ctx context.Context, sessionKey string) map[string]history.AsyncTask
	SetAsyncTasks(ctx context.Context, sessionKey string, tasks map[string]history.AsyncTask)
	Save(ctx context.Context, sessionKey string) error
	// DeleteSession removes all in-memory and persisted state for sessionKey.
	// Used to clean up ephemeral sub-agent and async-worker sessions after they
	// complete so they do not accumulate in storage. Deleting a non-existent
	// session is a no-op (returns nil).
	DeleteSession(ctx context.Context, sessionKey string) error
	// Fork creates a new session whose message history is a copy of the first
	// atIndex messages from sessionKey. atIndex is clamped to the source length
	// and snapped backward to a "safe" boundary — the resulting prefix never
	// ends mid tool-call / tool-result group, so the forked session is always
	// valid input to the agent loop. The behavior summary is copied; async
	// tasks are not (they belong to the original session runtime).
	//
	// Returns the generated new session key. Fails if sessionKey does not exist
	// or atIndex is negative.
	Fork(ctx context.Context, sessionKey string, atIndex int) (string, error)
}

// PendingToolCall represents a single tool invocation requested by the LLM.
type PendingToolCall struct {
	ID       string
	Name     string
	ArgsJSON string
}

// TokenUsage carries per-call token accounting returned by an LLM provider.
// Providers that do not report usage leave the fields zero.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// LLMResult represents the structured response from the LLM provider.
// Content is optional — the agent loop accumulates content from "content" stream events.
// Providers may still set Content for backward compatibility, but it is only used as a
// fallback when no content events were emitted on the stream.
// ToolCalls must be populated when the model wants to invoke tools.
// Usage carries token accounting when the provider reports it.
type LLMResult struct {
	Content   string
	ToolCalls []PendingToolCall
	Usage     TokenUsage
}

// BeforeLLMHook fires immediately before each GenerateStream call. Returning
// an error aborts the iteration (the loop emits an error event and exits).
// Use for per-session budget enforcement, rate limiting, or policy checks.
// estimatedTokens is a rough (chars/4) pre-call estimate of the prompt size.
type BeforeLLMHook func(ctx context.Context, sessionKey string, estimatedTokens int) error

// LLMProvider abstracts the model backend (OpenAI/Gemini/Claude).
type LLMProvider interface {
	GenerateStream(ctx context.Context, memory []history.Message, availableTools *tools.Registry, streamChan chan<- StreamEvent) (LLMResult, error)
}

// AgentLoop orchestrates the ReAct loop.
type AgentLoop struct {
	Sessions     SessionManager
	Tools        *tools.Registry
	LLM          LLMProvider
	MaxIters     int
	EmitThoughts bool
	BeforeHooks  []Hook

	// ConfirmHITL is called when a tool requires human approval.
	// If nil, HITL tools are auto-denied with a rejection message.
	// If set, the loop blocks until the function returns.
	ConfirmHITL ConfirmFunc

	// Cache provides optional tool result caching with trigram similarity matching.
	// If nil, caching is disabled. Set via cache.NewSearchCache().
	Cache *cache.SearchCache

	// MaxTokenBudget is the estimated token ceiling for the conversation context.
	// 0 disables budget enforcement. When the estimated token count exceeds this
	// value, the loop applies more aggressive context pruning before the next LLM
	// call. Estimation uses a 4-chars/token heuristic (~±20% accuracy for English).
	MaxTokenBudget int

	// EventHandlers is called for every StreamEvent emitted during the loop.
	// Multiple handlers can be registered; all fire in order for each event.
	// Register handlers via OnEvent for a fluent API.
	EventHandlers []EventHandler

	// Retry controls exponential-backoff retry for transient LLM errors.
	// nil disables retries. Use DefaultRetryConfig() for sensible defaults.
	// Retries are skipped when content has already started streaming to the client.
	Retry *RetryConfig

	// BeforeLLMHooks fire right before every GenerateStream call (including
	// retries). Any non-nil error aborts the iteration. Useful for per-session
	// spend caps and policy enforcement; pair with a BudgetTracker for a
	// ready-made implementation.
	BeforeLLMHooks []BeforeLLMHook
}

// NewAgentLoop creates a new agent with the given session manager, tool registry, and LLM provider.
// Optional hooks run before each iteration for security/policy enforcement.
func NewAgentLoop(sessions SessionManager, registry *tools.Registry, llm LLMProvider, hooks ...Hook) *AgentLoop {
	return &AgentLoop{
		Sessions:     sessions,
		Tools:        registry,
		LLM:          llm,
		MaxIters:     15,
		EmitThoughts: true,
		BeforeHooks:  hooks,
	}
}

// OnEvent registers an EventHandler and returns the AgentLoop for fluent chaining.
//
//	loop.OnEvent(metrics.Track).OnEvent(logger.Log)
func (al *AgentLoop) OnEvent(h EventHandler) *AgentLoop {
	al.EventHandlers = append(al.EventHandlers, h)
	return al
}

// emit sends ev to streamChan and fires all registered EventHandlers.
// It must only be called from within the runLogicLoop goroutine.
func (al *AgentLoop) emit(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent, ev StreamEvent) {
	streamChan <- ev
	for _, h := range al.EventHandlers {
		h(ctx, sessionKey, ev)
	}
}

// StreamEvent represents a chunk of data sent via SSE.
// When Type is "error", Err holds the structured error so callers can use errors.Is/As.
type StreamEvent struct {
	Type    string `json:"type"` // "content", "thought", "tool_call", "tool_progress", "action_required", "usage", "error", "done"
	Content string `json:"content"`
	Err     error  `json:"-"`
}

// errEvent is a convenience constructor for error StreamEvents with a typed error.
func errEvent(err error) StreamEvent {
	return StreamEvent{Type: "error", Content: err.Error(), Err: err}
}

// RunIteration provides a fast, blocking response interface (non-streaming).
// Thoughts are always suppressed — only final content and errors are returned.
func (al *AgentLoop) RunIteration(ctx context.Context, sessionKey string, userInput string) (string, error) {
	streamChan := make(chan StreamEvent, 100)

	go al.runLogicLoop(ctx, sessionKey, userInput, streamChan)

	var buf strings.Builder
	var lastErr error
	for ev := range streamChan {
		switch ev.Type {
		case "content":
			buf.WriteString(ev.Content)
		case "error":
			if ev.Err != nil {
				lastErr = ev.Err
			} else {
				lastErr = fmt.Errorf("agent: %s", ev.Content)
			}
		}
	}
	return buf.String(), lastErr
}

// RunIterationStream is a streaming version of the LLM loop that pushes data to a channel.
// It supports SSE by streaming thoughts and response chunks.
func (al *AgentLoop) RunIterationStream(ctx context.Context, sessionKey string, userInput string, streamChan chan<- StreamEvent) {
	internalChan := make(chan StreamEvent, 50)
	emitThoughts := al.EmitThoughts

	go func() {
		defer close(streamChan)
		for ev := range internalChan {
			if ev.Type == "thought" && !emitThoughts {
				continue
			}
			select {
			case streamChan <- ev:
			case <-ctx.Done():
				// Consumer disconnected — drain internalChan to unblock runLogicLoop
				for range internalChan {
				}
				return
			}
		}
	}()

	go al.runLogicLoop(ctx, sessionKey, userInput, internalChan)
}

// estimateTokens returns a rough token count using the 4-chars/token heuristic.
// Accurate within ~20% for English text; sufficient for MaxTokenBudget enforcement.
func estimateTokens(msgs []history.Message) int {
	var total int
	for _, m := range msgs {
		total += len(m.Content) / 4
		for _, tc := range m.ToolCalls {
			total += len(tc.Arguments) / 4
		}
	}
	return total
}

// saveSession persists history, using background context if the request context is already cancelled.
func (al *AgentLoop) saveSession(_ context.Context, sessionKey string, msgs []history.Message) {
	// Always use Background context — session persistence must outlive the HTTP request.
	saveCtx := context.Background()
	al.Sessions.SetHistory(saveCtx, sessionKey, msgs)
	if err := al.Sessions.Save(saveCtx, sessionKey); err != nil {
		log.Printf("[gopheragent] session save error for %q: %v", sessionKey, err)
	}
}

// SessionKey string is the type used to inject sessionKey into context
type SessionKeyCtx string

// runLogicLoop holds the actual execution context separated from the stream proxy
func (al *AgentLoop) runLogicLoop(ctx context.Context, sessionKey string, userInput string, streamChan chan<- StreamEvent) {
	defer close(streamChan)
	ctx = context.WithValue(ctx, SessionKeyCtx("sessionKey"), sessionKey)

	loopTracker := NewLoopDetector()

	for _, hook := range al.BeforeHooks {
		if hook == nil {
			continue
		}
		if err := hook(ctx, sessionKey, userInput); err != nil {
			al.emit(ctx, sessionKey, streamChan, errEvent(&HookRejectedError{Cause: err}))
			return
		}
	}

	msgs := append(al.Sessions.GetHistory(ctx, sessionKey), history.Message{Role: "user", Content: userInput})
	msgs = PatchDanglingToolCalls(msgs)
	al.Sessions.SetHistory(ctx, sessionKey, msgs)

	for iteration := 0; iteration < al.MaxIters; iteration++ {
		if ctx.Err() != nil {
			al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("%w: %w", ErrContextCancelled, ctx.Err())))
			al.saveSession(ctx, sessionKey, msgs)
			return
		}

		// Token budget enforcement
		if al.MaxTokenBudget > 0 {
			estToks := estimateTokens(msgs)
			thresh85 := int(float64(al.MaxTokenBudget) * 0.85)
			
			if estToks > thresh85 && estToks <= al.MaxTokenBudget {
				al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: fmt.Sprintf("Token budget near threshold (~%d >= %d). Truncating tool arguments.", estToks, thresh85)})
				msgs = TruncateToolArguments(msgs)
			}
			
			if estimateTokens(msgs) > al.MaxTokenBudget {
				al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: fmt.Sprintf(
					"Token budget exceeded (~%d est. tokens). Applying emergency context pruning.", estimateTokens(msgs),
				)})
				msgs = PruneContextMessages(msgs, 1)
			} else {
				msgs = PruneContextMessages(msgs, 3)
			}
		} else {
			msgs = PruneContextMessages(msgs, 3)
		}

		if iteration == al.MaxIters-2 {
			al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: "System: Nudging Agent for a soft landing before MaxIters limit is reached."})
			msgs = append(msgs, history.Message{
				Role:    "system",
				Content: "[System] You are approaching the iteration limit. Please wrap up and provide the final answer to the user immediately.",
			})
			al.Sessions.SetHistory(ctx, sessionKey, msgs)
		}

		// callLLM taps a fresh providerChan, forwards events to streamChan, and
		// accumulates content. Returns (finalContent, result, contentWasEmitted, err).
		// Retry is safe only when contentWasEmitted == false.
		callLLM := func() (string, LLMResult, bool, error) {
			// BeforeLLMHooks: any non-nil error aborts this call (no retry).
			for _, h := range al.BeforeLLMHooks {
				if h == nil {
					continue
				}
				if err := h(ctx, sessionKey, estimateTokens(msgs)); err != nil {
					return "", LLMResult{}, false, err
				}
			}

			pChan := make(chan StreamEvent, 50)
			var buf strings.Builder
			var emitted bool
			done := make(chan struct{})
			go func() {
				defer close(done)
				for ev := range pChan {
					if ev.Type == "content" {
						buf.WriteString(ev.Content)
						emitted = true
					}
					select {
					case streamChan <- ev:
						for _, h := range al.EventHandlers {
							h(ctx, sessionKey, ev)
						}
					case <-ctx.Done():
						for range pChan {
						}
						return
					}
				}
			}()
			res, err := al.LLM.GenerateStream(ctx, msgs, al.Tools, pChan)
			close(pChan)
			<-done
			content := buf.String()
			if content == "" {
				content = res.Content
			}
			// Emit a usage event when the provider reported token accounting.
			if err == nil && res.Usage.TotalTokens > 0 {
				payload, _ := json.Marshal(res.Usage)
				al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "usage", Content: string(payload)})
			}
			return content, res, emitted, err
		}

		finalContent, result, _, err := callLLM()

		// Retry on transient errors — only when no content has been streamed yet.
		if err != nil && al.Retry != nil && isRetryable(err) {
			for attempt := 0; attempt < al.Retry.MaxRetries; attempt++ {
				delay := al.Retry.delay(attempt)
				al.emit(ctx, sessionKey, streamChan, StreamEvent{
					Type:    "thought",
					Content: fmt.Sprintf("LLM error (%v). Retrying in %s (attempt %d/%d)...", err, delay, attempt+1, al.Retry.MaxRetries),
				})
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					err = ctx.Err()
					goto doneRetry
				}
				var retryContent string
				var contentEmitted bool
				retryContent, result, contentEmitted, err = callLLM()
				if retryContent != "" {
					finalContent = retryContent
				}
				if err == nil || contentEmitted || !isRetryable(err) {
					break
				}
			}
		}
	doneRetry:

		if err != nil {
			al.emit(ctx, sessionKey, streamChan, errEvent(&LLMFailureError{Cause: err}))
			al.saveSession(ctx, sessionKey, msgs)
			return
		}

		if len(result.ToolCalls) == 0 {
			al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "done"})
			msgs = append(msgs, history.Message{Role: "assistant", Content: finalContent})
			al.saveSession(ctx, sessionKey, msgs)
			return
		}

		assistantMsg := history.Message{Role: "assistant", Content: finalContent}
		for _, tc := range result.ToolCalls {
			assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, history.ToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: tc.ArgsJSON,
			})
		}
		msgs = append(msgs, assistantMsg)
		al.Sessions.SetHistory(ctx, sessionKey, msgs)

		var wg sync.WaitGroup
		toolMsgs := make([]history.Message, len(result.ToolCalls))
		var fatalErr error
		var fatalMu sync.Mutex
		var hitlMu sync.Mutex

		for i, tc := range result.ToolCalls {
			wg.Add(1)
			go func(idx int, tCall PendingToolCall) {
				defer wg.Done()

				fatalMu.Lock()
				hasFatal := fatalErr != nil
				fatalMu.Unlock()
				if hasFatal {
					return
				}

				tool, ok := al.Tools.Get(tCall.Name)
				if !ok {
					toolErr := &ToolNotFoundError{ToolName: tCall.Name}
					al.emit(ctx, sessionKey, streamChan, errEvent(toolErr))
					toolMsgs[idx] = history.Message{
						Role:       "tool",
						Content:    toolErr.Error(),
						ToolCallID: tCall.ID,
						IsError:    true,
					}
					return
				}

				if tool.RequiresConfirmation() {
					approved := func() bool {
						hitlMu.Lock()
						defer hitlMu.Unlock()

						al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: "CRITICAL: Tool requires human confirmation."})

						// When no custom handler is registered, emit action_required here
						// so the frontend still knows a confirmation was needed.
						// When ConfirmHITL is set, the handler is responsible for emitting
						// action_required (with any metadata such as approval_id).
						appr := false
						if al.ConfirmHITL != nil {
							appr = al.ConfirmHITL(ctx, tCall.Name, tCall.ArgsJSON)
						} else {
							payload, _ := json.Marshal(map[string]string{"tool": tCall.Name, "args": tCall.ArgsJSON})
							al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "action_required", Content: string(payload)})
						}
						return appr
					}()

					if !approved {
						deniedErr := &HITLDeniedError{ToolName: tCall.Name}
						toolMsgs[idx] = history.Message{
							Role:       "tool",
							Content:    fmt.Sprintf("%v. Do not attempt this action again. Find a workaround.", deniedErr),
							ToolCallID: tCall.ID,
							IsError:    true,
						}
						return
					}
					al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: "Human APPROVED tool execution."})
				}

				cacheKey := tCall.Name + ":" + tCall.ArgsJSON
				if al.Cache != nil {
					if cached, hit := al.Cache.Get(cacheKey); hit {
						al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: fmt.Sprintf("Cache hit for %s, skipping execution.", tCall.Name)})
						toolMsgs[idx] = history.Message{Role: "tool", Content: cached, ToolCallID: tCall.ID}
						return
					}
				}

				al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "tool_call", Content: fmt.Sprintf("Executing: %s", tCall.Name)})

				// Inject a progress reporter so the tool can emit status updates
				// during long-running operations (polling, downloads, etc.) without
				// taking a dependency on the streaming layer.
				//
				// Sends are non-blocking: progress is inherently lossy, and a
				// high-frequency emitter (byte counters, SQL row counts) must
				// not be able to stall the tool goroutine if the SSE consumer
				// backs up.
				toolCtx := tools.WithProgressFunc(ctx, func(msg string) {
					ev := StreamEvent{Type: "tool_progress", Content: msg}
					select {
					case streamChan <- ev:
						for _, h := range al.EventHandlers {
							h(ctx, sessionKey, ev)
						}
					default:
						// Consumer is backed up; drop this progress tick.
					}
				})
				toolResult, execErr := tool.Execute(toolCtx, tCall.ArgsJSON)
				content := toolResult
				isToolErr := execErr != nil
				if isToolErr {
					content = fmt.Sprintf("Error: %v", execErr)
				}

				if !isToolErr {
					if ir, ok := tool.(tools.InlineRenderer); ok && ir.InlineResult() {
						al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "content", Content: "\n\n" + content + "\n\n"})
					}
				}

				if al.Cache != nil && !isToolErr {
					al.Cache.Put(cacheKey, content)
				}

				loopTracker.AddCall(tCall.Name, tCall.ArgsJSON, content)
				warnMessage, loopErr := loopTracker.Detect()
				if loopErr != nil {
					fatalMu.Lock()
					if fatalErr == nil {
						fatalErr = loopErr
					}
					fatalMu.Unlock()
					return
				}
				if warnMessage != "" {
					content += "\n\n" + warnMessage
					al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: "System inserted an anti-loop warning into context window."})
				}

				toolMsgs[idx] = history.Message{
					Role:       "tool",
					Content:    content,
					ToolCallID: tCall.ID,
					IsError:    isToolErr,
				}
			}(i, tc)
		}

		wg.Wait()

		if fatalErr != nil {
			al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("%w: %w", ErrLoopDetected, fatalErr)))
			al.saveSession(ctx, sessionKey, msgs)
			return
		}

		for _, m := range toolMsgs {
			if m.Role != "" {
				msgs = append(msgs, m)
			}
		}
		al.Sessions.SetHistory(ctx, sessionKey, msgs)
	}

	al.emit(ctx, sessionKey, streamChan, errEvent(ErrMaxIterations))
	al.saveSession(ctx, sessionKey, msgs)
}
