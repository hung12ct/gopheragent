package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// GenerateJSONRequest is the input to GenerateJSON. Messages follow the
// same shape the AgentLoop passes to an LLMProvider — typically a system
// message plus a user message describing the task.
type GenerateJSONRequest struct {
	// Messages is the conversation to send. Must contain at least one user
	// message. Keep it short: this is a one-shot call, not a loop.
	Messages []history.Message
	// Output describes the JSON shape the caller wants back. Schema must
	// be non-empty; Name and Description are forwarded when the provider
	// supports them (OpenAI, Anthropic synthesizer).
	Output StructuredOutput
}

// GenerateJSON runs a single schema-constrained generation against the
// provider, bypassing the ReAct loop entirely. Use this when you just
// need "LLM, give me JSON matching this shape" without tools or iteration.
//
// The returned string is the raw JSON body as emitted by the provider —
// use GenerateJSONInto[T] below when you already have a Go type for the
// response.
//
// GenerateJSON never writes to disk, never touches SessionManager, and
// never invokes tools. The only ctx interaction is StructuredOutput
// injection (WithStructuredOutput) and whatever the provider itself
// reads from ctx (thinking budget, etc).
func GenerateJSON(ctx context.Context, provider LLMProvider, req GenerateJSONRequest) (string, LLMResult, error) {
	if provider == nil {
		return "", LLMResult{}, fmt.Errorf("agent: GenerateJSON: provider is nil")
	}
	if len(req.Messages) == 0 {
		return "", LLMResult{}, fmt.Errorf("agent: GenerateJSON: messages is empty")
	}
	if len(req.Output.Schema) == 0 {
		return "", LLMResult{}, fmt.Errorf("agent: GenerateJSON: Output.Schema is empty")
	}

	ctx = WithStructuredOutput(ctx, req.Output)

	// Drain the stream channel — callers of GenerateJSON do not watch
	// streaming events, they want the final JSON. Buffered so a provider
	// that spams chunks does not block waiting for a reader.
	stream := make(chan StreamEvent, 64)
	done := make(chan struct{})
	go func() {
		for range stream {
		}
		close(done)
	}()

	result, err := provider.GenerateStream(ctx, req.Messages, nil, stream)
	close(stream)
	<-done
	if err != nil {
		return "", result, fmt.Errorf("agent: GenerateJSON: %w", err)
	}
	if result.Content == "" {
		return "", result, fmt.Errorf("agent: GenerateJSON: provider returned empty content (ToolCalls=%d)", len(result.ToolCalls))
	}
	return result.Content, result, nil
}

// GenerateJSONInto runs GenerateJSON and unmarshals the result into out.
// out must be a pointer to the target type. Returns the provider's
// LLMResult (usage / etc.) alongside any decoding error.
func GenerateJSONInto(ctx context.Context, provider LLMProvider, req GenerateJSONRequest, out any) (LLMResult, error) {
	raw, result, err := GenerateJSON(ctx, provider, req)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return result, fmt.Errorf("agent: GenerateJSON: decode %q: %w", raw, err)
	}
	return result, nil
}
