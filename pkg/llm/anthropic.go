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
			systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: m.Content})
		case "user":
			if len(m.Parts) > 0 {
				messages = append(messages, anthropic.NewUserMessage(anthropicBlocksFromMediaParts(m.Content, m.Parts)...))
			} else {
				messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
			}
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
				messages = append(messages, anthropic.MessageParam{Role: anthropic.MessageParamRoleAssistant, Content: blocks})
			} else {
				messages = append(messages, anthropic.MessageParam{
					Role:    anthropic.MessageParamRoleAssistant,
					Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(m.Content)},
				})
			}
		case "tool":
			messages = append(messages, anthropic.NewUserMessage(
				anthropic.NewToolResultBlock(m.ToolCallID, m.Content, m.IsError),
			))
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
