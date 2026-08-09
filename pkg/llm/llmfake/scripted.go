// Package llmfake provides in-tree fakes for the agent.LLMProvider
// interface, mirroring pkg/history/historyfake for the session layer.
// Adopter tests that need to drive an agent through a deterministic
// sequence of turns reach for ScriptedProvider instead of hand-rolling
// a one-off fake on every test file.
package llmfake

import (
	"context"
	"fmt"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// Turn declares what a single GenerateStream call should return. Set
// exactly one of Content / ToolCalls / Func / Err — the precedence is:
//
//  1. Func — when non-nil, it owns the full response and other fields
//     on the Turn are ignored. Use for dynamic behavior (inspect the
//     incoming messages, branch on tool result, simulate streaming).
//  2. Err — when non-nil, returned verbatim. Other fields ignored.
//  3. ToolCalls — when non-empty, returned as the result's tool calls.
//     Content is also returned (typically empty for tool turns).
//  4. Content — returned as the assistant text. Emitted as a single
//     ContentEvent on the stream channel so RunIteration's accumulator
//     picks it up exactly the way a real provider's output would.
//
// Usage records the TokenUsage to attach. Leave zero for "no usage
// reported" — the agent loop's UsageEvent is then suppressed.
type Turn struct {
	Content   string
	ToolCalls []agent.PendingToolCall
	Usage     agent.TokenUsage
	Err       error
	Func      func(ctx context.Context, msgs []history.Message, registry *tools.Registry, stream chan<- agent.StreamEvent) (agent.LLMResult, error)
}

// ScriptedProvider walks through Turns one per GenerateStream call.
// Once the script is exhausted, subsequent calls return DefaultTurn
// (which defaults to a benign "done" reply); set Strict=true to make
// over-runs fail the test loudly instead.
//
// Safe for concurrent use; a single mutex serializes the index bump so
// parallel callers each get exactly one turn in script order.
type ScriptedProvider struct {
	// Turns is the ordered script. Each GenerateStream call consumes
	// one entry.
	Turns []Turn
	// DefaultTurn is returned once Turns is exhausted (when Strict is
	// false). Zero value is "Content: 'done'" which lets most tests
	// terminate cleanly without scripting the final turn.
	DefaultTurn Turn
	// Strict, when true, makes GenerateStream return an error if the
	// script is exhausted. Use for tests that want to assert "exactly
	// N turns were taken."
	Strict bool

	mu  sync.Mutex
	idx int
}

var _ agent.CapabilityProvider = (*ScriptedProvider)(nil)

// Capabilities reports the zero value: the scripted fake honours neither
// image parts nor schema-enforced structured output. It replays its script
// verbatim and never reads the incoming messages' MediaParts, so a Turn's
// Content comes back whether or not an image was attached.
//
// Declaring that explicitly is the point. A multimodal consumer handed this
// fake gets a well-formed, confident answer produced without the model ever
// seeing an image — indistinguishable from success in a log — and this is
// what lets it reject the provider at construction instead.
func (*ScriptedProvider) Capabilities() agent.LLMCapabilities {
	return agent.LLMCapabilities{}
}

// GenerateStream implements agent.LLMProvider. See Turn for the
// dispatch rules.
func (p *ScriptedProvider) GenerateStream(ctx context.Context, msgs []history.Message, registry *tools.Registry, stream chan<- agent.StreamEvent) (agent.LLMResult, error) {
	p.mu.Lock()
	if p.idx >= len(p.Turns) {
		p.mu.Unlock()
		if p.Strict {
			return agent.LLMResult{}, fmt.Errorf("llmfake: script exhausted after %d turns", len(p.Turns))
		}
		return p.dispatch(ctx, p.defaultTurn(), msgs, registry, stream)
	}
	turn := p.Turns[p.idx]
	p.idx++
	p.mu.Unlock()
	return p.dispatch(ctx, turn, msgs, registry, stream)
}

// dispatch resolves one Turn into an LLMResult and emits any
// corresponding stream events. Extracted so GenerateStream stays the
// thin guard around p.mu and the dispatch body has no locking concerns.
func (p *ScriptedProvider) dispatch(ctx context.Context, turn Turn, msgs []history.Message, registry *tools.Registry, stream chan<- agent.StreamEvent) (agent.LLMResult, error) {
	if turn.Func != nil {
		return turn.Func(ctx, msgs, registry, stream)
	}
	if turn.Err != nil {
		return agent.LLMResult{}, turn.Err
	}
	if turn.Content != "" {
		stream <- agent.Event(agent.ContentEvent{Text: turn.Content})
	}
	return agent.LLMResult{
		Content:   turn.Content,
		ToolCalls: turn.ToolCalls,
		Usage:     turn.Usage,
	}, nil
}

// defaultTurn returns DefaultTurn with a built-in fallback when the
// caller didn't set one — "Content: done". Keeps Strict=false tests
// from hanging forever on an exhausted script.
func (p *ScriptedProvider) defaultTurn() Turn {
	if p.DefaultTurn.Content == "" && p.DefaultTurn.Err == nil && len(p.DefaultTurn.ToolCalls) == 0 && p.DefaultTurn.Func == nil {
		return Turn{Content: "done"}
	}
	return p.DefaultTurn
}

// Reset rewinds the script to the start. Useful for tests that reuse
// the same ScriptedProvider across multiple sub-cases.
func (p *ScriptedProvider) Reset() {
	p.mu.Lock()
	p.idx = 0
	p.mu.Unlock()
}

// TurnsTaken returns the number of turns consumed so far. Concurrent
// callers see a monotonically-increasing counter; the value at any
// instant reflects all serialized GenerateStream calls that returned.
func (p *ScriptedProvider) TurnsTaken() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idx
}
