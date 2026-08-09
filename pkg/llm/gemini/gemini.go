// Package gemini implements agent.LLMProvider for Google Gemini — both
// the public Gemini API and Vertex AI — plus the Gemini-backed embedder
// and multimodal media analyzer.
package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"google.golang.org/genai"
)

// Provider implements agent.LLMProvider using Google Gemini native SDK.
type Provider struct {
	client      *genai.Client
	model       string
	temperature *float64
	topP        *float64
	seed        *int64
}

var _ agent.CapabilityProvider = (*Provider)(nil)

// Capabilities reports features implemented by the Gemini adapter.
func (*Provider) Capabilities() agent.LLMCapabilities {
	return agent.LLMCapabilities{ImageInput: true, StructuredOutput: true}
}

// Option configures a Provider at construction.
type Option func(*Provider)

// WithTemperature pins the sampling temperature (0.0–2.0 for Gemini).
// Use 0 for maximally reproducible classification/extraction turns; unset
// keeps the provider default.
func WithTemperature(t float64) Option {
	return func(p *Provider) { p.temperature = &t }
}

// WithTopP pins nucleus sampling. Unset keeps the provider default.
func WithTopP(v float64) Option {
	return func(p *Provider) { p.topP = &v }
}

// WithSeed requests best-effort deterministic sampling. Gemini accepts a
// 32-bit seed; values outside int32 range are truncated. Determinism is
// not guaranteed across model versions — pair with a pinned temperature
// for the strongest reproducibility Gemini offers.
func WithSeed(n int64) Option {
	return func(p *Provider) { p.seed = &n }
}

// New creates a wrapper over Google Gemini's API.
func New(apiKey string, model string, opts ...Option) (*Provider, error) {
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("GEMINI_API_KEY is not set in environment")
	}
	if model == "" {
		model = "gemini-2.5-flash"
	}

	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey: apiKey,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: create client: %w", err)
	}

	p := &Provider{
		client: client,
		model:  model,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// GenerateStream maps GopherAgent history to Gemini API format and triggers streaming generation.
func (p *Provider) GenerateStream(ctx context.Context, memory []history.Message, availableTools *tools.Registry, streamChan chan<- agent.StreamEvent) (agent.LLMResult, error) {
	var contents []*genai.Content
	var systemInstruction *genai.Content

	for _, m := range memory {
		if m.Role == "system" {
			systemInstruction = &genai.Content{
				Parts: []*genai.Part{{Text: m.Content}},
			}
			continue
		}

		var role string
		var parts []*genai.Part

		switch m.Role {
		case "user":
			role = "user"
			if len(m.Parts) > 0 {
				parts = append(parts, partsFromMediaParts(m.Content, m.Parts)...)
			} else {
				parts = append(parts, &genai.Part{Text: m.Content})
			}
		case "assistant":
			role = "model"
			if m.Content != "" {
				parts = append(parts, &genai.Part{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var argsMap map[string]any
				if tc.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Arguments), &argsMap)
				}
				parts = append(parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{
						Name: tc.Name,
						Args: argsMap,
					},
				})
			}
		case "tool":
			role = "user" // Tool responses are sent as 'user' role with FunctionResponse part
			var resultObj map[string]any
			// Try parsing as JSON so the tool response is structured, fallback to wrapped text
			if err := json.Unmarshal([]byte(m.Content), &resultObj); err != nil {
				resultObj = map[string]any{"result": m.Content}
			}
			parts = append(parts, &genai.Part{
				FunctionResponse: &genai.FunctionResponse{
					Name:     m.ToolCallID, // For gemini we need to match the func name here. We rely on ToolCallID housing the function name in this simple mapping or use a default.
					Response: resultObj,
				},
			})
		}
		if role != "" && len(parts) > 0 {
			contents = append(contents, &genai.Content{
				Role:  role,
				Parts: parts,
			})
		}
	}

	config := &genai.GenerateContentConfig{
		SystemInstruction: systemInstruction,
	}
	p.applySampling(config)

	applyStructuredOutput(config, agent.StructuredOutputFromContext(ctx))

	if availableTools != nil {
		var geminiTools []*genai.Tool
		var funcDecls []*genai.FunctionDeclaration
		for _, t := range availableTools.All() {
			desc := t.Descriptor()
			funcDecls = append(funcDecls, &genai.FunctionDeclaration{
				Name:        desc.Name,
				Description: desc.Description,
				Parameters:  mapRegistrySchemaToGeminiSchema(desc.Parameters),
			})
		}
		if len(funcDecls) > 0 {
			geminiTools = append(geminiTools, &genai.Tool{
				FunctionDeclarations: funcDecls,
			})
			config.Tools = geminiTools
		}
	}

	iter := p.client.Models.GenerateContentStream(ctx, p.model, contents, config)

	streamChan <- agent.Event(agent.ThoughtEvent{Message: "Analyzing with Gemini..."})

	var finalContent string
	var pendingCalls []agent.PendingToolCall
	var usage agent.TokenUsage
	var finishReason genai.FinishReason

	for resp, err := range iter {
		if err != nil {
			return agent.LLMResult{}, classifyErr(err)
		}

		if resp == nil {
			continue
		}
		if resp.UsageMetadata != nil {
			usage = agent.TokenUsage{
				PromptTokens:     int(resp.UsageMetadata.PromptTokenCount),
				CompletionTokens: int(resp.UsageMetadata.CandidatesTokenCount),
				TotalTokens:      int(resp.UsageMetadata.TotalTokenCount),
			}
		}
		if len(resp.Candidates) == 0 {
			continue
		}
		// Read the reason before the Content guard below: a stream stopped
		// by a content filter carries the reason on a candidate whose
		// Content is nil, so skipping early would drop the only signal.
		if r := resp.Candidates[0].FinishReason; r != "" {
			finishReason = r
		}
		if resp.Candidates[0].Content == nil {
			continue
		}

		for _, part := range resp.Candidates[0].Content.Parts {
			if part.Text != "" {
				finalContent += part.Text
				streamChan <- agent.Event(agent.ContentEvent{Text: part.Text})
			}
			if part.FunctionCall != nil {
				argsBytes, _ := json.Marshal(part.FunctionCall.Args)
				pendingCalls = append(pendingCalls, agent.PendingToolCall{
					ID:       part.FunctionCall.Name, // Gemini doesn't use explicit call IDs, using name as ID is a common fallback
					Name:     part.FunctionCall.Name,
					ArgsJSON: string(argsBytes),
				})
			}
		}
	}

	result := agent.LLMResult{
		Content:   finalContent,
		ToolCalls: pendingCalls,
		Usage:     usage,
	}
	// A non-STOP finish reason means the accumulated text is a prefix, not
	// an answer. Returning it as a success is what turns a truncation into
	// a decode error several layers up, naming the caller's schema instead
	// of the real cause. The partial rides on the result for adopters that
	// want to show what arrived.
	if err := finishReasonErr(finishReason); err != nil {
		if finishReason == genai.FinishReasonMaxTokens {
			streamChan <- agent.LimitExhaustedStreamEvent(agent.LimitKindProviderMaxTokens, 0, usage.CompletionTokens)
		}
		return result, err
	}
	return result, nil
}

// applySampling stamps the configured temperature/top_p/seed onto config.
func (p *Provider) applySampling(config *genai.GenerateContentConfig) {
	if p.temperature != nil {
		config.Temperature = genai.Ptr(float32(*p.temperature))
	}
	if p.topP != nil {
		config.TopP = genai.Ptr(float32(*p.topP))
	}
	if p.seed != nil {
		config.Seed = genai.Ptr(int32(*p.seed))
	}
}

// partsFromMediaParts converts MediaParts into Gemini's native Part
// slice. Inline raw bytes become InlineData blobs; URLs become FileData
// entries (Gemini Files API / Cloud Storage URIs, or plain https for
// gemini-2.5-flash and newer).
//
// A non-empty caption is prepended as a text part so prompts travel
// alongside the media. MIME defaults to image/png when unspecified.
func partsFromMediaParts(caption string, parts []history.MediaPart) []*genai.Part {
	out := make([]*genai.Part, 0, len(parts)+1)
	if caption != "" {
		out = append(out, &genai.Part{Text: caption})
	}
	for _, p := range parts {
		switch p.Type {
		case history.PartText:
			if p.Text == "" {
				continue
			}
			out = append(out, &genai.Part{Text: p.Text})
		case history.PartImage:
			mime := p.MIME
			if mime == "" {
				mime = "image/png"
			}
			if len(p.Data) > 0 {
				out = append(out, &genai.Part{
					InlineData: &genai.Blob{MIMEType: mime, Data: p.Data},
				})
			} else if p.URL != "" {
				out = append(out, &genai.Part{
					FileData: &genai.FileData{FileURI: p.URL, MIMEType: mime},
				})
			}
		}
	}
	return out
}

// applyStructuredOutput translates the ctx-carried StructuredOutput
// request to Gemini's native response_mime_type + response_json_schema
// fields. Nil so is a no-op (default path, wire semantics unchanged).
// Name/Description/Strict have no analog on Gemini — the API exposes no
// schema-name field and enforces the schema unconditionally.
func applyStructuredOutput(config *genai.GenerateContentConfig, so *agent.StructuredOutput) {
	if so == nil || len(so.Schema) == 0 {
		return
	}
	config.ResponseMIMEType = "application/json"
	config.ResponseJsonSchema = so.Schema
}

// mapRegistrySchemaToGeminiSchema converts a tool's JSON-Schema parameters
// into Gemini's *genai.Schema. Gemini strict-validates the schema, so we must
// forward array items, nested object properties / required, and enums — not
// just type and description.
func mapRegistrySchemaToGeminiSchema(s tools.ToolSchema) *genai.Schema {
	if s.Type == "" {
		return nil
	}
	schema := &genai.Schema{Type: jsonTypeToGeminiType(s.Type)}
	if len(s.Required) > 0 {
		schema.Required = s.Required
	}
	if s.Properties != nil {
		schema.Properties = make(map[string]*genai.Schema, len(s.Properties))
		for k, v := range s.Properties {
			vMap, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if child := propMapToGeminiSchema(vMap); child != nil {
				schema.Properties[k] = child
			}
		}
	}
	return schema
}

// propMapToGeminiSchema recursively maps a single JSON-Schema property
// fragment to *genai.Schema. Handles array items, nested object properties,
// required lists, and enums in both []string and []any forms.
func propMapToGeminiSchema(vMap map[string]any) *genai.Schema {
	typeStr, _ := vMap["type"].(string)
	if typeStr == "" {
		return nil
	}
	out := &genai.Schema{Type: jsonTypeToGeminiType(typeStr)}
	if d, ok := vMap["description"].(string); ok {
		out.Description = d
	}
	if items, ok := vMap["items"].(map[string]any); ok {
		out.Items = propMapToGeminiSchema(items)
	}
	if props, ok := vMap["properties"].(map[string]any); ok {
		out.Properties = make(map[string]*genai.Schema, len(props))
		for pk, pv := range props {
			pvMap, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			if child := propMapToGeminiSchema(pvMap); child != nil {
				out.Properties[pk] = child
			}
		}
	}
	out.Required = stringSliceFromAny(vMap["required"])
	out.Enum = stringSliceFromAny(vMap["enum"])
	return out
}

func jsonTypeToGeminiType(t string) genai.Type {
	switch t {
	case "string":
		return genai.TypeString
	case "integer":
		return genai.TypeInteger
	case "number":
		return genai.TypeNumber
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	case "object":
		return genai.TypeObject
	}
	return genai.TypeObject
}

// stringSliceFromAny accepts []string (the canonical SchemaFor output) or
// []any (hand-built schemas) and returns the strings; nil otherwise.
func stringSliceFromAny(v any) []string {
	switch x := v.(type) {
	case []string:
		if len(x) == 0 {
			return nil
		}
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}
