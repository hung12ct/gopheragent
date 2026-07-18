package openai

import (
	"context"
	"fmt"
	"os"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// VisionAnalyzer calls a vision-capable OpenAI model (default gpt-4o)
// and returns a natural-language answer about the supplied image.
//
// It satisfies pkg/tools/builtin.MediaAnalyzer by structural compatibility —
// no import of that package is needed.
//
// media may be:
//   - an https:// URL publicly reachable by the OpenAI API
//   - a data URI ("data:image/png;base64,...")  for locally hosted images
type VisionAnalyzer struct {
	client *openai.Client
	model  string
}

// NewVisionAnalyzer builds an analyzer. apiKey defaults to
// OPENAI_API_KEY; model defaults to "gpt-4o" (smallest model with
// reliable vision support across all image tasks).
func NewVisionAnalyzer(apiKey, model string) (*VisionAnalyzer, error) {
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("openai: OPENAI_API_KEY not set")
	}
	if model == "" {
		model = "gpt-4o"
	}
	return &VisionAnalyzer{client: openai.NewClient(apiKey), model: model}, nil
}

// Analyze sends media + prompt to the vision model and returns the response.
func (a *VisionAnalyzer) Analyze(ctx context.Context, media, prompt string) (string, error) {
	resp, err := a.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: a.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role: openai.ChatMessageRoleUser,
				MultiContent: []openai.ChatMessagePart{
					{
						Type: openai.ChatMessagePartTypeImageURL,
						ImageURL: &openai.ChatMessageImageURL{
							URL:    media,
							Detail: openai.ImageURLDetailAuto,
						},
					},
					{
						Type: openai.ChatMessagePartTypeText,
						Text: prompt,
					},
				},
			},
		},
		MaxTokens: 2048,
	})
	if err != nil {
		return "", fmt.Errorf("openai: vision: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openai: vision: no choices returned")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}
