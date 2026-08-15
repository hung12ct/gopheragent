package agent

import (
	"context"
	"encoding/json"
)

// StructuredOutput is a provider-neutral request for a JSON-schema-constrained
// response. Providers translate it to their closest native knob:
//
//   - OpenAI (gpt-4o-*, o-series): response_format: {type:"json_schema",
//     json_schema:{name, description, schema, strict}}. Native and strict.
//     Compatible endpoints publishing only the older {type:"json_object"}
//     declare it with openai.WithJSONMode, which moves the schema into the
//     prompt: still JSON, but conformance stops being enforced server-side,
//     so validate the result.
//   - Gemini (1.5+): response_mime_type:"application/json" +
//     response_schema. Native; Strict is ignored (Gemini always enforces).
//   - Anthropic Claude: no native JSON mode. Providers synthesize a single
//     tool with the given Schema and force tool_choice, then surface the
//     tool's Input as the assistant's Content. Transparent to callers.
//
// Set via WithStructuredOutput before calling GenerateStream (or use the
// GenerateJSON helper for one-shot generation outside the ReAct loop).
//
// Schema must be a JSON-Schema object; callers typically build it with
// SchemaForStruct[T] or hand-write a map[string]any. The exact dialect
// understood differs by provider: OpenAI and Gemini accept a generous
// subset of JSON-Schema 2020-12; when in doubt, stick to type/properties/
// required/items/enum and you will be fine on all three backends.
type StructuredOutput struct {
	// Name is a short identifier for the schema. OpenAI requires this
	// (it surfaces in validation errors). Other providers ignore it.
	Name string
	// Description is optional free-text explaining the schema's intent.
	// Forwarded to providers that expose a description field.
	Description string
	// Schema is the JSON-Schema object describing the expected response.
	// Must be non-nil — an empty schema is not the same as "no schema" and
	// will be sent as-is to the provider.
	Schema map[string]any
	// Strict asks providers to reject any field not declared in Schema.
	// OpenAI honors this via json_schema.strict=true; Gemini enforces
	// strictly regardless; Anthropic's tool-synthesis path is implicitly
	// strict (the tool only accepts the declared shape).
	Strict bool
}

type structuredOutputKey struct{}

// WithStructuredOutput returns ctx with a schema-constrained response
// request attached. Nil / empty Schema is treated as "no hint" and clears
// any previously-set request.
//
// The AgentLoop installs this before every GenerateStream call when its
// StructuredOutput field is set; provider adapters read it with
// StructuredOutputFromContext.
func WithStructuredOutput(ctx context.Context, so StructuredOutput) context.Context {
	if len(so.Schema) == 0 {
		return context.WithValue(ctx, structuredOutputKey{}, (*StructuredOutput)(nil))
	}
	copy := so
	return context.WithValue(ctx, structuredOutputKey{}, &copy)
}

// StructuredOutputFromContext returns the schema-constrained request stored
// on ctx, or nil when none is set. Provider adapters call this right before
// building the request and skip the JSON-mode fields when nil.
func StructuredOutputFromContext(ctx context.Context) *StructuredOutput {
	v, _ := ctx.Value(structuredOutputKey{}).(*StructuredOutput)
	return v
}

// MarshalSchema returns a canonical JSON encoding of the schema, useful for
// providers that want the schema as a raw JSON string rather than a map.
// Returns nil, nil when so is nil or has no schema.
func (so *StructuredOutput) MarshalSchema() ([]byte, error) {
	if so == nil || len(so.Schema) == 0 {
		return nil, nil
	}
	return json.Marshal(so.Schema)
}
