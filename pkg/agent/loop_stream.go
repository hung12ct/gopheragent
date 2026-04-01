// Package agent provides the core ReAct loop, streaming, and orchestration for LLM agents.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
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
type SessionManager interface {
	GetHistory(ctx context.Context, sessionKey string) []history.Message
	SetHistory(ctx context.Context, sessionKey string, messages []history.Message)
	Save(ctx context.Context, sessionKey string) error
}

// PendingToolCall represents a single tool invocation requested by the LLM.
type PendingToolCall struct {
	ID       string
	Name     string
	ArgsJSON string
}

// LLMResult represents the structured response from the LLM provider.
// Content is optional — the agent loop accumulates content from "content" stream events.
// Providers may still set Content for backward compatibility, but it is only used as a
// fallback when no content events were emitted on the stream.
// ToolCalls must be populated when the model wants to invoke tools.
type LLMResult struct {
	Content   string
	ToolCalls []PendingToolCall
}

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
	Type    string `json:"type"` // "content", "thought", "tool_call", "action_required", "error", "done"
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
func (al *AgentLoop) saveSession(ctx context.Context, sessionKey string, msgs []history.Message) {
	saveCtx := ctx
	if ctx.Err() != nil {
		saveCtx = context.Background()
	}
	al.Sessions.SetHistory(saveCtx, sessionKey, msgs)
	if err := al.Sessions.Save(saveCtx, sessionKey); err != nil {
		log.Printf("[gopheragent] session save error for %q: %v", sessionKey, err)
	}
}

// runLogicLoop holds the actual execution context separated from the stream proxy
func (al *AgentLoop) runLogicLoop(ctx context.Context, sessionKey string, userInput string, streamChan chan<- StreamEvent) {
	defer close(streamChan)

	loopTracker := NewLoopDetector()

	for _, hook := range al.BeforeHooks {
		if err := hook(ctx, sessionKey, userInput); err != nil {
			al.emit(ctx, sessionKey, streamChan, errEvent(&HookRejectedError{Cause: err}))
			return
		}
	}

	msgs := append(al.Sessions.GetHistory(ctx, sessionKey), history.Message{Role: "user", Content: userInput})
	al.Sessions.SetHistory(ctx, sessionKey, msgs)

	for iteration := 0; iteration < al.MaxIters; iteration++ {
		if ctx.Err() != nil {
			al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("%w: %w", ErrContextCancelled, ctx.Err())))
			al.saveSession(ctx, sessionKey, msgs)
			return
		}

		// Token budget enforcement: if over budget, prune more aggressively (keep 1).
		if al.MaxTokenBudget > 0 && estimateTokens(msgs) > al.MaxTokenBudget {
			al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: fmt.Sprintf(
				"Token budget exceeded (~%d est. tokens). Applying emergency context pruning.", estimateTokens(msgs),
			)})
			msgs = PruneContextMessages(msgs, 1)
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

		for _, tc := range result.ToolCalls {
			tool, ok := al.Tools.Get(tc.Name)
			if !ok {
				toolErr := &ToolNotFoundError{ToolName: tc.Name}
				al.emit(ctx, sessionKey, streamChan, errEvent(toolErr))
				msgs = append(msgs, history.Message{
					Role:       "tool",
					Content:    toolErr.Error(),
					ToolCallID: tc.ID,
					IsError:    true,
				})
				al.Sessions.SetHistory(ctx, sessionKey, msgs)
				continue
			}

			if tool.RequiresConfirmation() {
				al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: "CRITICAL: Tool requires human confirmation."})

				payload, _ := json.Marshal(map[string]string{"tool": tc.Name, "args": tc.ArgsJSON})
				al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "action_required", Content: string(payload)})

				approved := false
				if al.ConfirmHITL != nil {
					approved = al.ConfirmHITL(ctx, tc.Name, tc.ArgsJSON)
				}

				if !approved {
					deniedErr := &HITLDeniedError{ToolName: tc.Name}
					msgs = append(msgs, history.Message{
						Role:       "tool",
						Content:    fmt.Sprintf("%v. Do not attempt this action again. Find a workaround.", deniedErr),
						ToolCallID: tc.ID,
						IsError:    true,
					})
					al.Sessions.SetHistory(ctx, sessionKey, msgs)
					continue
				}
				al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: "Human APPROVED tool execution."})
			}

			cacheKey := tc.Name + ":" + tc.ArgsJSON
			if al.Cache != nil {
				if cached, hit := al.Cache.Get(cacheKey); hit {
					al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: fmt.Sprintf("Cache hit for %s, skipping execution.", tc.Name)})
					msgs = append(msgs, history.Message{Role: "tool", Content: cached, ToolCallID: tc.ID})
					al.Sessions.SetHistory(ctx, sessionKey, msgs)
					continue
				}
			}

			al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "tool_call", Content: fmt.Sprintf("Executing: %s", tc.Name)})

			toolResult, execErr := tool.Execute(ctx, tc.ArgsJSON)
			content := toolResult
			isToolErr := execErr != nil
			if isToolErr {
				content = fmt.Sprintf("Error: %v", execErr)
			}

			if al.Cache != nil && !isToolErr {
				al.Cache.Put(cacheKey, content)
			}

			loopTracker.AddCall(tc.Name, tc.ArgsJSON, content)
			warnMessage, loopErr := loopTracker.Detect()
			if loopErr != nil {
				al.emit(ctx, sessionKey, streamChan, errEvent(fmt.Errorf("%w: %w", ErrLoopDetected, loopErr)))
				al.saveSession(ctx, sessionKey, msgs)
				return
			}
			if warnMessage != "" {
				content += "\n\n" + warnMessage
				al.emit(ctx, sessionKey, streamChan, StreamEvent{Type: "thought", Content: "System inserted an anti-loop warning into context window."})
			}

			msgs = append(msgs, history.Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: tc.ID,
				IsError:    isToolErr,
			})
			al.Sessions.SetHistory(ctx, sessionKey, msgs)
		}
	}

	al.emit(ctx, sessionKey, streamChan, errEvent(ErrMaxIterations))
	al.saveSession(ctx, sessionKey, msgs)
}
