package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// DefaultAnthropicMaxTokens is the per-call Anthropic MaxTokens used when
// no override is provided. 8192 catches typical chat + tool-use turns
// without truncating most code-gen workloads. Bump via WithMaxTokens for
// long structured outputs (HTML5 playables, full schema dumps,
// inline charts) — Sonnet 4.6 supports up to 64K.
const DefaultAnthropicMaxTokens int64 = 8192

// AnthropicProvider implements agent.LLMProvider using the Anthropic Messages API (Claude).
type AnthropicProvider struct {
	client    *anthropic.Client
	model     anthropic.Model
	MaxTokens int64
}

// AnthropicOption configures an AnthropicProvider at construction.
type AnthropicOption func(*AnthropicProvider)

// WithMaxTokens overrides the per-call Anthropic MaxTokens. Use for
// code-generation, long structured output, or any workload where the
// default truncates mid-stream.
func WithMaxTokens(n int64) AnthropicOption {
	return func(p *AnthropicProvider) { p.MaxTokens = n }
}

// NewAnthropicProvider creates a Claude-backed provider.
// Auto-discovers ANTHROPIC_API_KEY from environment if apiKey is empty.
func NewAnthropicProvider(apiKey string, model string, opts ...AnthropicOption) (*AnthropicProvider, error) {
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("ANTHROPIC_API_KEY is not set in environment")
	}
	m := anthropic.Model(model)
	if model == "" {
		m = anthropic.ModelClaudeSonnet4_6
	}
	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	p := &AnthropicProvider{client: &client, model: m, MaxTokens: DefaultAnthropicMaxTokens}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func (p *AnthropicProvider) GenerateStream(ctx context.Context, memory []history.Message, availableTools *tools.Registry, streamChan chan<- agent.StreamEvent) (agent.LLMResult, error) {
	var systemBlocks []anthropic.TextBlockParam
	var messages []anthropic.MessageParam

	for _, m := range memory {
		switch m.Role {
		case "system":
			block := anthropic.TextBlockParam{Text: m.Content}
			if m.CacheHint {
				block.CacheControl = anthropic.NewCacheControlEphemeralParam()
			}
			systemBlocks = append(systemBlocks, block)
		case "user":
			var blocks []anthropic.ContentBlockParamUnion
			if len(m.Parts) > 0 {
				blocks = anthropicBlocksFromMediaParts(m.Content, m.Parts)
			} else {
				blocks = []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(m.Content)}
			}
			if m.CacheHint && len(blocks) > 0 {
				stampCacheControl(&blocks[len(blocks)-1])
			}
			messages = append(messages, anthropic.NewUserMessage(blocks...))
		case "assistant":
			if len(m.ToolCalls) > 0 {
				var blocks []anthropic.ContentBlockParamUnion
				if m.Content != "" {
					blocks = append(blocks, anthropic.NewTextBlock(m.Content))
				}
				for _, tc := range m.ToolCalls {
					var input map[string]any
					if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
						return agent.LLMResult{}, fmt.Errorf("corrupt tool call args for %s: %w", tc.Name, err)
					}
					blocks = append(blocks, anthropic.ContentBlockParamUnion{
						OfToolUse: &anthropic.ToolUseBlockParam{
							ID:    tc.ID,
							Name:  tc.Name,
							Input: input,
						},
					})
				}
				if m.CacheHint && len(blocks) > 0 {
					stampCacheControl(&blocks[len(blocks)-1])
				}
				messages = append(messages, anthropic.MessageParam{Role: anthropic.MessageParamRoleAssistant, Content: blocks})
			} else {
				blocks := []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(m.Content)}
				if m.CacheHint {
					stampCacheControl(&blocks[0])
				}
				messages = append(messages, anthropic.MessageParam{
					Role:    anthropic.MessageParamRoleAssistant,
					Content: blocks,
				})
			}
		case "tool":
			block := anthropic.NewToolResultBlock(m.ToolCallID, m.Content, m.IsError)
			if m.CacheHint {
				stampCacheControl(&block)
			}
			messages = append(messages, anthropic.NewUserMessage(block))
		}
	}

	var anthropicTools []anthropic.ToolUnionParam
	if availableTools != nil {
		for _, t := range availableTools.GetAll() {
			schema := t.ParametersSchema()
			anthropicTools = append(anthropicTools, anthropic.ToolUnionParam{
				OfTool: &anthropic.ToolParam{
					Name:        t.Name(),
					Description: anthropic.String(t.Description()),
					InputSchema: anthropic.ToolInputSchemaParam{
						Properties: schema.Properties,
						Required:   schema.Required,
					},
				},
			})
		}
	}

	// Anthropic has no native JSON mode — synthesize a single tool whose
	// input schema matches the requested shape and force the model to call
	// it. The tool's Input is surfaced back to the caller as .Content on the
	// LLMResult so Mode A / GenerateJSON consumers see a plain JSON string
	// and never learn about the tool trick.
	so := agent.StructuredOutputFromContext(ctx)
	structuredToolName := ""
	if so != nil && len(so.Schema) > 0 {
		tool, name := synthesizeAnthropicStructuredTool(so)
		anthropicTools = append(anthropicTools, tool)
		structuredToolName = name
	}

	params := anthropic.MessageNewParams{
		Model:     p.model,
		MaxTokens: p.MaxTokens,
		Messages:  messages,
	}
	if len(systemBlocks) > 0 {
		params.System = systemBlocks
	}
	if len(anthropicTools) > 0 {
		params.Tools = anthropicTools
	}
	if structuredToolName != "" {
		params.ToolChoice = anthropic.ToolChoiceParamOfTool(structuredToolName)
	}
	if b := resolveThinkingBudget(ctx, p.MaxTokens); b > 0 {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(b)
	}

	streamChan <- agent.StreamEvent{Type: agent.EventTypeThought, Content: fmt.Sprintf("Analyzing with Claude (%s)...", p.model)}

	stream := p.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	accumulated := anthropic.Message{}
	for stream.Next() {
		event := stream.Current()
		if err := accumulated.Accumulate(event); err != nil {
			return agent.LLMResult{}, fmt.Errorf("anthropic accumulate error: %w", err)
		}

		switch variant := event.AsAny().(type) {
		case anthropic.ContentBlockDeltaEvent:
			switch delta := variant.Delta.AsAny().(type) {
			case anthropic.TextDelta:
				if delta.Text != "" {
					streamChan <- agent.StreamEvent{Type: agent.EventTypeContent, Content: delta.Text}
				}
			}
		case anthropic.ContentBlockStopEvent:
			// A content block just finalized. If it was a tool_use block,
			// the full {id, name, input} is available on `accumulated`
			// right now — emit tool_call_ready so the agent loop can
			// speculatively start safe tools while the response keeps
			// streaming. Any downstream consumer that doesn't care simply
			// ignores the event type.
			idx := int(variant.Index)
			if idx >= 0 && idx < len(accumulated.Content) {
				if tu, ok := accumulated.Content[idx].AsAny().(anthropic.ToolUseBlock); ok {
					argsBytes, err := json.Marshal(tu.Input)
					if err == nil {
						payload, pErr := json.Marshal(struct {
							ID   string `json:"id"`
							Name string `json:"name"`
							Args string `json:"args"`
						}{ID: tu.ID, Name: tu.Name, Args: string(argsBytes)})
						if pErr == nil {
							streamChan <- agent.StreamEvent{Type: agent.EventTypeToolCallReady, Content: string(payload)}
						}
					}
				}
			}
		}
	}

	if stream.Err() != nil {
		return agent.LLMResult{}, fmt.Errorf("anthropic stream error: %w", stream.Err())
	}

	// Surface the per-call MaxTokens truncation as a typed cap event so
	// adopters can render "model truncated; raise MaxTokens" instead of
	// silently shipping half-rendered code blocks.
	if string(accumulated.StopReason) == "max_tokens" {
		streamChan <- agent.LimitExhaustedStreamEvent(agent.LimitKindProviderMaxTokens, int(p.MaxTokens), 0)
	}

	var finalContent string
	var pendingCalls []agent.PendingToolCall
	for _, block := range accumulated.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			finalContent += v.Text
		case anthropic.ToolUseBlock:
			argsBytes, err := json.Marshal(v.Input)
			if err != nil {
				return agent.LLMResult{}, fmt.Errorf("failed to marshal tool input for %s: %w", v.Name, err)
			}
			// When the caller requested structured output, Anthropic answers
			// by calling the synthesized tool — its Input IS the JSON the
			// caller asked for. Surface it as Content so downstream code
			// (GenerateJSON, etc.) sees the same shape OpenAI and Gemini
			// return natively, and do not leak the tool call upstream.
			if structuredToolName != "" && v.Name == structuredToolName {
				finalContent = string(argsBytes)
				continue
			}
			pendingCalls = append(pendingCalls, agent.PendingToolCall{
				ID:       v.ID,
				Name:     v.Name,
				ArgsJSON: string(argsBytes),
			})
		}
	}

	usage := agent.TokenUsage{
		PromptTokens:     int(accumulated.Usage.InputTokens),
		CompletionTokens: int(accumulated.Usage.OutputTokens),
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	return agent.LLMResult{Content: finalContent, ToolCalls: pendingCalls, Usage: usage}, nil
}

// synthesizeAnthropicStructuredTool builds a fake tool whose InputSchema
// mirrors the user's StructuredOutput and returns it alongside the tool
// name the caller should pass to ToolChoiceParamOfTool. Anthropic exposes
// no native JSON mode, so forcing the model to call this tool is the
// standard trick for schema-constrained output.
//
// Top-level type/properties/required are extracted into the typed fields;
// any other JSON-Schema keywords (additionalProperties, $defs, oneOf, …)
// ride along via ExtraFields, which the SDK merges at marshal time.
func synthesizeAnthropicStructuredTool(so *agent.StructuredOutput) (anthropic.ToolUnionParam, string) {
	name := so.Name
	if name == "" {
		name = "structured_response"
	}
	inputSchema := anthropic.ToolInputSchemaParam{}
	extras := map[string]any{}
	for k, v := range so.Schema {
		switch k {
		case "properties":
			inputSchema.Properties = v
		case "required":
			switch req := v.(type) {
			case []string:
				inputSchema.Required = req
			case []any:
				out := make([]string, 0, len(req))
				for _, e := range req {
					if s, ok := e.(string); ok {
						out = append(out, s)
					}
				}
				inputSchema.Required = out
			}
		case "type":
			// ToolInputSchemaParam.Type is pinned to "object" — anything
			// else from the caller is dropped silently. That matches the
			// practical reality: Anthropic's tool inputs must be objects.
		default:
			extras[k] = v
		}
	}
	if len(extras) > 0 {
		inputSchema.ExtraFields = extras
	}
	description := so.Description
	if description == "" {
		description = "Return the response as JSON matching the provided schema."
	}
	tool := anthropic.ToolUnionParam{
		OfTool: &anthropic.ToolParam{
			Name:        name,
			Description: anthropic.String(description),
			InputSchema: inputSchema,
		},
	}
	return tool, name
}

// resolveThinkingBudget clamps a caller-supplied extended-thinking budget
// to what the Anthropic API accepts for the current request.
//
// Returns 0 (disabled) when the context carries no budget or when maxTokens
// is too small to fit the 1024-token floor alongside any output. Otherwise
// the budget is raised to at least 1024 and lowered to at most maxTokens-1
// so the API call does not 400 on "budget_tokens must be < max_tokens".
func resolveThinkingBudget(ctx context.Context, maxTokens int64) int64 {
	b := int64(agent.ThinkingBudgetFromContext(ctx))
	if b <= 0 {
		return 0
	}
	const minBudget int64 = 1024
	if maxTokens <= minBudget {
		return 0
	}
	if b < minBudget {
		b = minBudget
	}
	if b >= maxTokens {
		b = maxTokens - 1
	}
	return b
}

// stampCacheControl marks a ContentBlockParamUnion as an Anthropic
// prompt-cache breakpoint. The request will cache every content block from
// the start of the prompt up to and including this one for ~5 minutes;
// subsequent requests that reuse the same prefix hit the cache at ~10% of
// the normal input-token cost. A request can carry up to 4 breakpoints.
func stampCacheControl(block *anthropic.ContentBlockParamUnion) {
	cc := anthropic.NewCacheControlEphemeralParam()
	switch {
	case block.OfText != nil:
		block.OfText.CacheControl = cc
	case block.OfToolUse != nil:
		block.OfToolUse.CacheControl = cc
	case block.OfToolResult != nil:
		block.OfToolResult.CacheControl = cc
	case block.OfImage != nil:
		block.OfImage.CacheControl = cc
	case block.OfDocument != nil:
		block.OfDocument.CacheControl = cc
	}
}

// anthropicBlocksFromMediaParts converts MediaParts into the Anthropic
// ContentBlockParamUnion sequence. A non-empty caption is prepended as a
// text block so plain prompts like "What's in this image?" ride along with
// the image.
//
// Raw bytes are base64-encoded; URLs pass through as URLImageSourceParam.
// Parts with no usable payload are silently skipped (the alternative —
// surfacing an error — would force every caller to pre-validate, which is
// more trouble than it's worth for a format issue).
func anthropicBlocksFromMediaParts(caption string, parts []history.MediaPart) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(parts)+1)
	if caption != "" {
		blocks = append(blocks, anthropic.NewTextBlock(caption))
	}
	for _, p := range parts {
		switch p.Type {
		case history.PartText:
			if p.Text == "" {
				continue
			}
			blocks = append(blocks, anthropic.NewTextBlock(p.Text))
		case history.PartImage:
			if len(p.Data) > 0 {
				mime := p.MIME
				if mime == "" {
					mime = "image/png"
				}
				blocks = append(blocks, anthropic.NewImageBlockBase64(mime, base64.StdEncoding.EncodeToString(p.Data)))
			} else if p.URL != "" {
				blocks = append(blocks, anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: p.URL}))
			}
		}
	}
	return blocks
}
