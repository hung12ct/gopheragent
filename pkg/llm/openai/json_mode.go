package openai

import (
	"context"
	"fmt"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/sashabaranov/go-openai"
)

// JSONMode selects the wire encoding an endpoint accepts for a
// schema-constrained response.
//
// api.openai.com takes response_format {type:"json_schema"} with the schema
// inline and enforces it server-side. Compatible endpoints implement an
// unknown subset of that: several publish only the older
// {type:"json_object"}, which guarantees JSON syntax but carries no schema
// field at all, and some publish neither. Sending json_schema to an endpoint
// that does not implement it is a 400 on every structured call, so the mode
// is configuration — the adapter cannot infer it from a base URL.
type JSONMode int

const (
	// JSONModeSchema sends response_format {type:"json_schema", ...} with the
	// schema inline, and the endpoint enforces it. Zero value because it is
	// what this package is named for; NewCompat overrides it.
	JSONModeSchema JSONMode = iota
	// JSONModeObject sends response_format {type:"json_object"} and appends
	// the schema to the prompt, because that wire format has nowhere to put
	// it. The endpoint guarantees only that the reply parses as JSON —
	// conformance to the schema degrades to a model instruction, so callers
	// should validate the result and be ready to retry.
	JSONModeObject
	// JSONModeNone declares no JSON mode. A structured-output request against
	// such an endpoint fails at the call, rather than silently downgrading to
	// free-form prose that the caller would then try to unmarshal.
	JSONModeNone
)

// String implements fmt.Stringer so the mode reads as its constant name in
// error messages instead of an integer.
func (m JSONMode) String() string {
	switch m {
	case JSONModeSchema:
		return "JSONModeSchema"
	case JSONModeObject:
		return "JSONModeObject"
	case JSONModeNone:
		return "JSONModeNone"
	default:
		return fmt.Sprintf("JSONMode(%d)", int(m))
	}
}

// WithJSONMode declares which JSON mode this endpoint implements. It is
// meaningful only on a compatible endpoint: New already reports the mode
// api.openai.com implements, and NewCompat claims none until this says
// otherwise.
//
// It also drives Capabilities().StructuredOutput, so declaring the mode is
// what lets a consumer that depends on schema enforcement accept the
// provider at construction.
func WithJSONMode(m JSONMode) Option {
	return providerOptionFunc(func(p *Provider) { p.jsonMode = m })
}

// WithImageInput declares whether this endpoint accepts image parts on user
// messages. Same reasoning as WithJSONMode: New reports OpenAI's own
// support, NewCompat claims none until the caller declares it.
//
// It sets Capabilities().ImageInput only. Media parts are rendered on the
// wire either way, so this changes what the provider promises, not what it
// sends — a caller that skips the capability check still reaches the
// endpoint's own rejection.
func WithImageInput(ok bool) Option {
	return providerOptionFunc(func(p *Provider) { p.imageInput = ok })
}

// structuredOutputFor translates the provider-neutral structured-output
// request on ctx into this endpoint's JSON mode. It returns the
// response_format to send plus the messages to send with it, since
// JSONModeObject carries the schema as an extra message. Both are returned
// unchanged when no structured output was requested.
func (p *Provider) structuredOutputFor(
	ctx context.Context,
	msgs []openai.ChatCompletionMessage,
) (*openai.ChatCompletionResponseFormat, []openai.ChatCompletionMessage, error) {
	so := agent.StructuredOutputFromContext(ctx)
	if so == nil {
		return nil, msgs, nil
	}
	switch p.jsonMode {
	case JSONModeSchema:
		name := so.Name
		if name == "" {
			name = "response"
		}
		return &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:        name,
				Description: so.Description,
				Schema:      jsonSchemaMarshaler(so.Schema),
				Strict:      so.Strict,
			},
		}, msgs, nil
	case JSONModeObject:
		instruction, err := jsonObjectInstruction(so)
		if err != nil {
			return nil, nil, err
		}
		// Appended last rather than merged into an existing system message:
		// the schema is an instruction, and the final message is the one
		// models follow most reliably.
		return &openai.ChatCompletionResponseFormat{
				Type: openai.ChatCompletionResponseFormatTypeJSONObject,
			}, append(msgs, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleSystem,
				Content: instruction,
			}), nil
	default:
		return nil, nil, fmt.Errorf(
			"openai: structured output requested but this endpoint declares %s: pass WithJSONMode(JSONModeSchema) or WithJSONMode(JSONModeObject) to the constructor",
			p.jsonMode)
	}
}

// jsonObjectInstruction renders the schema as a prompt fragment, which is
// where it has to live under JSONModeObject.
//
// The literal word JSON is load-bearing, not decoration: endpoints
// implementing this format commonly reject a request whose messages never
// mention it, to stop callers from switching on JSON mode and then asking
// for prose.
func jsonObjectInstruction(so *agent.StructuredOutput) (string, error) {
	schema, err := so.MarshalSchema()
	if err != nil {
		return "", fmt.Errorf("openai: encoding structured-output schema: %w", err)
	}
	var b strings.Builder
	b.Grow(len(schema) + 256)
	b.WriteString("Reply with a single JSON object and nothing else: no prose, no code fence.\n")
	b.WriteString("It must validate against this JSON Schema:\n")
	b.Write(schema)
	if so.Description != "" {
		b.WriteString("\nThe object represents: ")
		b.WriteString(so.Description)
	}
	if so.Strict {
		b.WriteString("\nDo not emit any property the schema does not declare.")
	}
	return b.String(), nil
}
