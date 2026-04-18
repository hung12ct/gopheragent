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

// AnthropicProvider implements agent.LLMProvider using the Anthropic Messages API (Claude).
type AnthropicProvider struct {
	client    *anthropic.Client
	model     anthropic.Model
	MaxTokens int64
}

// NewAnthropicProvider creates a Claude-backed provider.
// Auto-discovers ANTHROPIC_API_KEY from environment if apiKey is empty.
func NewAnthropicProvider(apiKey string, model string) (*AnthropicProvider, error) {
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
	return &AnthropicProvider{client: &client, model: m, MaxTokens: 4096}, nil
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
	if b := resolveThinkingBudget(ctx, p.MaxTokens); b > 0 {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(b)
	}

	streamChan <- agent.StreamEvent{Type: "thought", Content: fmt.Sprintf("Analyzing with Claude (%s)...", p.model)}

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
					streamChan <- agent.StreamEvent{Type: "content", Content: delta.Text}
				}
			}
		}
	}

	if stream.Err() != nil {
		return agent.LLMResult{}, fmt.Errorf("anthropic stream error: %w", stream.Err())
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
