// Package llm provides LLM provider implementations for OpenAI, Anthropic, and OpenAI-compatible APIs.
package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"github.com/sashabaranov/go-openai"
)

// jsonSchemaMarshaler adapts a map[string]any to json.Marshaler so it can be
// handed to openai.ChatCompletionResponseFormatJSONSchema.Schema.
type jsonSchemaMarshaler map[string]any

func (m jsonSchemaMarshaler) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any(m))
}

// OpenAIProvider implements agent.LLMProvider using the OpenAI Chat Completions API.
type OpenAIProvider struct {
	client *openai.Client
	model  string
}

// NewOpenAIProvider creates a wrapper over OpenAI's API.
// Auto-discovers OPENAI_API_KEY from environment if apiKey is empty.
func NewOpenAIProvider(apiKey string, model string) (*OpenAIProvider, error) {
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("OPENAI_API_KEY is not set in environment")
	}
	if model == "" {
		model = openai.GPT4o
	}
	return &OpenAIProvider{
		client: openai.NewClient(apiKey),
		model:  model,
	}, nil
}

// GenerateStream maps GopherAgent history to OpenAI API format and triggers streaming generation.
func (p *OpenAIProvider) GenerateStream(ctx context.Context, memory []history.Message, availableTools *tools.Registry, streamChan chan<- agent.StreamEvent) (agent.LLMResult, error) {
	reqMessages := make([]openai.ChatCompletionMessage, 0, len(memory))

	for _, m := range memory {
		msg := openai.ChatCompletionMessage{
			Role:    m.Role,
			Content: m.Content,
		}
		// Multimodal: when Parts is set we must populate MultiContent
		// instead of Content (OpenAI rejects both being set on the same
		// message — see ErrContentFieldsMisused in the SDK).
		if len(m.Parts) > 0 && m.Role == "user" {
			msg.Content = ""
			msg.MultiContent = openAIPartsFromMediaParts(m.Content, m.Parts)
		}
		// tool result: needs ToolCallID
		if m.Role == "tool" && m.ToolCallID != "" {
			msg.ToolCallID = m.ToolCallID
		}
		// assistant message that invoked tools: needs ToolCalls array
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
		}
		reqMessages = append(reqMessages, msg)
	}

	var openaiTools []openai.Tool
	if availableTools != nil {
		for _, t := range availableTools.All() {
			desc := t.Descriptor()
			schema := desc.Parameters
			// OpenAI requires "required" to be an array, never null.
			required := schema.Required
			if required == nil {
				required = []string{}
			}
			openaiTools = append(openaiTools, openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        desc.Name,
					Description: desc.Description,
					Parameters: map[string]any{
						"type":       schema.Type,
						"properties": schema.Properties,
						"required":   required,
					},
				},
			})
		}
	}

	req := openai.ChatCompletionRequest{
		Model:    p.model,
		Messages: reqMessages,
		Tools:    openaiTools,
		Stream:   true,
		StreamOptions: &openai.StreamOptions{
			IncludeUsage: true,
		},
	}
	if effort := reasoningEffortFor(p.model, agent.ThinkingBudgetFromContext(ctx)); effort != "" {
		req.ReasoningEffort = effort
	}
	if so := agent.StructuredOutputFromContext(ctx); so != nil {
		name := so.Name
		if name == "" {
			name = "response"
		}
		req.ResponseFormat = &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:        name,
				Description: so.Description,
				Schema:      jsonSchemaMarshaler(so.Schema),
				Strict:      so.Strict,
			},
		}
	}

	stream, err := p.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return agent.LLMResult{}, fmt.Errorf("openai streaming error: %w", err)
	}
	defer stream.Close()

	var finalContent string
	var usage agent.TokenUsage

	// Accumulate parallel tool calls by index (OpenAI streams them split across chunks)
	type toolCallAccum struct {
		id   string
		name string
		args string
	}
	toolCallMap := map[int]*toolCallAccum{}

	streamChan <- agent.Event(agent.ThoughtEvent{Message: "Analyzing with OpenAI..."})

	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return agent.LLMResult{}, fmt.Errorf("openai chunk read error: %w", err)
		}

		// The final chunk carries Usage when StreamOptions.IncludeUsage is set.
		if response.Usage != nil {
			usage = agent.TokenUsage{
				PromptTokens:     response.Usage.PromptTokens,
				CompletionTokens: response.Usage.CompletionTokens,
				TotalTokens:      response.Usage.TotalTokens,
			}
		}

		if len(response.Choices) == 0 {
			continue
		}
		delta := response.Choices[0].Delta

		if delta.Content != "" {
			finalContent += delta.Content
			streamChan <- agent.Event(agent.ContentEvent{Text: delta.Content})
		}

		for _, tc := range delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			if toolCallMap[idx] == nil {
				toolCallMap[idx] = &toolCallAccum{}
			}
			if tc.ID != "" {
				toolCallMap[idx].id = tc.ID
			}
			if tc.Function.Name != "" {
				toolCallMap[idx].name = tc.Function.Name
			}
			toolCallMap[idx].args += tc.Function.Arguments
		}
	}

	var pendingCalls []agent.PendingToolCall
	indices := make([]int, 0, len(toolCallMap))
	for k := range toolCallMap {
		indices = append(indices, k)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		tc := toolCallMap[idx]
		pendingCalls = append(pendingCalls, agent.PendingToolCall{
			ID:       tc.id,
			Name:     tc.name,
			ArgsJSON: tc.args,
		})
	}

	return agent.LLMResult{
		Content:   finalContent,
		ToolCalls: pendingCalls,
		Usage:     usage,
	}, nil
}

// reasoningEffortFor maps a generic ThinkingBudget token count to the
// reasoning_effort string the OpenAI chat API understands.
//
// Only reasoning-capable models receive the field: OpenAI rejects
// reasoning_effort on stock chat models like gpt-4o. The allow-list uses a
// prefix match on names that begin with "o1", "o3", "o4", or carry an
// explicit "reasoning" tag — anything else returns an empty string and the
// caller omits the field entirely.
//
// Thresholds are intentionally coarse: the API exposes only four steps
// ("minimal"/"low"/"medium"/"high"), so a token count is a better UX than
// asking callers to memorize magic strings.
func reasoningEffortFor(model string, budget int) string {
	if budget <= 0 {
		return ""
	}
	m := strings.ToLower(model)
	reasoning := strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") ||
		strings.HasPrefix(m, "o4") ||
		strings.Contains(m, "reasoning")
	if !reasoning {
		return ""
	}
	switch {
	case budget <= 2048:
		return "low"
	case budget <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// openAIPartsFromMediaParts converts GopherAgent's provider-neutral MediaPart
// slice into OpenAI's MultiContent representation. A leading text fragment
// derived from Content (when non-empty) keeps captions attached to images.
//
// For image parts, raw Data is folded into a data: URI — OpenAI accepts both
// https URLs and data: URIs interchangeably for image_url.
func openAIPartsFromMediaParts(caption string, parts []history.MediaPart) []openai.ChatMessagePart {
	out := make([]openai.ChatMessagePart, 0, len(parts)+1)
	if caption != "" {
		out = append(out, openai.ChatMessagePart{
			Type: openai.ChatMessagePartTypeText,
			Text: caption,
		})
	}
	for _, p := range parts {
		switch p.Type {
		case history.PartText:
			if p.Text == "" {
				continue
			}
			out = append(out, openai.ChatMessagePart{
				Type: openai.ChatMessagePartTypeText,
				Text: p.Text,
			})
		case history.PartImage:
			url := p.URL
			if url == "" && len(p.Data) > 0 {
				mime := p.MIME
				if mime == "" {
					mime = "image/png"
				}
				url = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
			}
			if url == "" {
				continue
			}
			out = append(out, openai.ChatMessagePart{
				Type:     openai.ChatMessagePartTypeImageURL,
				ImageURL: &openai.ChatMessageImageURL{URL: url},
			})
		}
	}
	return out
}
