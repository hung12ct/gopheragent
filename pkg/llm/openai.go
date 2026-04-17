// Package llm provides LLM provider implementations for OpenAI, Anthropic, and OpenAI-compatible APIs.
package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
	"github.com/sashabaranov/go-openai"
)

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
		for _, t := range availableTools.GetAll() {
			schema := t.ParametersSchema()
			// OpenAI requires "required" to be an array, never null.
			required := schema.Required
			if required == nil {
				required = []string{}
			}
			openaiTools = append(openaiTools, openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        t.Name(),
					Description: t.Description(),
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

	streamChan <- agent.StreamEvent{Type: "thought", Content: "Analyzing with OpenAI..."}

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
			streamChan <- agent.StreamEvent{Type: "content", Content: delta.Content}
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
