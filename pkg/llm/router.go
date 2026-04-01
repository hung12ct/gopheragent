package llm

import (
	"context"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// RouteCondition inspects the current conversation and returns true when the
// associated provider should handle the request.
type RouteCondition func(memory []history.Message) bool

// route pairs a condition with the provider to invoke when it matches.
type route struct {
	condition RouteCondition
	provider  agent.LLMProvider
}

// RouterProvider is an agent.LLMProvider that forwards each request to the first
// provider whose RouteCondition matches the conversation. Falls back to a default
// provider when no condition matches.
//
// Typical pattern — use a cheap/fast model for short or simple tasks:
//
//	router := llm.NewRouterProvider(powerfulProvider).
//	    AddRoute(llm.IfTokensUnder(500), fastProvider).
//	    AddRoute(llm.IfLastMessageContains("summarize", "tldr"), fastProvider)
//
// Register as the LLM for any AgentLoop:
//
//	loop := agent.NewAgentLoop(sessions, registry, router)
type RouterProvider struct {
	routes   []route
	fallback agent.LLMProvider
}

// NewRouterProvider creates a router with the given fallback provider.
// The fallback is used when no route condition matches.
func NewRouterProvider(fallback agent.LLMProvider) *RouterProvider {
	return &RouterProvider{fallback: fallback}
}

// AddRoute registers a condition+provider pair. Routes are evaluated in
// declaration order; the first match wins.
func (r *RouterProvider) AddRoute(condition RouteCondition, provider agent.LLMProvider) *RouterProvider {
	r.routes = append(r.routes, route{condition: condition, provider: provider})
	return r
}

// GenerateStream selects the appropriate provider and delegates.
func (r *RouterProvider) GenerateStream(
	ctx context.Context,
	memory []history.Message,
	availableTools *tools.Registry,
	streamChan chan<- agent.StreamEvent,
) (agent.LLMResult, error) {
	for _, rt := range r.routes {
		if rt.condition(memory) {
			return rt.provider.GenerateStream(ctx, memory, availableTools, streamChan)
		}
	}
	return r.fallback.GenerateStream(ctx, memory, availableTools, streamChan)
}

// ── Built-in RouteConditions ──────────────────────────────────────────────────

// IfTokensUnder returns true when the estimated token count of the conversation
// is below threshold. Uses the same 4-chars/token heuristic as the agent loop.
// Useful for routing short/cheap tasks to a fast model.
func IfTokensUnder(threshold int) RouteCondition {
	return func(memory []history.Message) bool {
		var total int
		for _, m := range memory {
			total += len(m.Content) / 4
			for _, tc := range m.ToolCalls {
				total += len(tc.Arguments) / 4
			}
		}
		return total < threshold
	}
}

// IfMessageCountUnder returns true when the conversation has fewer than n messages.
// Conversations with few turns are usually simple and can use a lighter model.
func IfMessageCountUnder(n int) RouteCondition {
	return func(memory []history.Message) bool {
		return len(memory) < n
	}
}

// IfLastMessageContains returns true when the most recent user message contains
// any of the given keywords (case-insensitive). Use for intent-based routing:
//
//	llm.IfLastMessageContains("summarize", "tldr", "shorten") → fastProvider
//	llm.IfLastMessageContains("analyze", "compare", "reason") → powerfulProvider
func IfLastMessageContains(keywords ...string) RouteCondition {
	return func(memory []history.Message) bool {
		for i := len(memory) - 1; i >= 0; i-- {
			if memory[i].Role != "user" {
				continue
			}
			lower := strings.ToLower(memory[i].Content)
			for _, kw := range keywords {
				if strings.Contains(lower, strings.ToLower(kw)) {
					return true
				}
			}
			break
		}
		return false
	}
}

// IfSystemPromptContains returns true when any system message contains the keyword.
// Useful for routing sub-agents by their declared role:
//
//	// In a sub-agent with system prompt "You are a summarizer…"
//	llm.IfSystemPromptContains("summarizer") → fastProvider
func IfSystemPromptContains(keyword string) RouteCondition {
	lower := strings.ToLower(keyword)
	return func(memory []history.Message) bool {
		for _, m := range memory {
			if m.Role == "system" && strings.Contains(strings.ToLower(m.Content), lower) {
				return true
			}
		}
		return false
	}
}

// Always returns a RouteCondition that always matches.
// Use it as the last route to make a provider the "second fallback":
//
//	router.AddRoute(llm.IfTokensUnder(500), fastProvider).
//	    AddRoute(llm.Always(), mediumProvider) // before the real fallback
func Always() RouteCondition {
	return func(_ []history.Message) bool { return true }
}
