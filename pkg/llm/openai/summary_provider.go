package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/sashabaranov/go-openai"
)

// SummaryProvider implements history.SummaryProvider using OpenAI.
// It uses a cheap, fast model (default: gpt-4o-mini) to extract long-term
// behavioral patterns from conversation history, injected back into the
// system prompt on the next turn.
//
// Usage:
//
//	sp, _ := openai.NewSummaryProvider("", "gpt-4o-mini")
//	sm := history.NewInMemSessionManager("You are an assistant.")
//	sm.SummaryProvider = sp
type SummaryProvider struct {
	client *openai.Client
	model  string
}

// NewSummaryProvider creates an OpenAI-backed SummaryProvider.
// Auto-discovers OPENAI_API_KEY from environment if apiKey is empty.
// model defaults to "gpt-4o-mini" (cheap + fast — ideal for background summarization).
// Pass WithBaseURL to summarize against an OpenAI-compatible endpoint
// instead of api.openai.com.
func NewSummaryProvider(apiKey, model string, opts ...ClientOption) (*SummaryProvider, error) {
	client, err := newClientFor(apiKey, "NewSummaryProvider", opts)
	if err != nil {
		return nil, err
	}
	if model == "" {
		model = openai.GPT4oMini
	}
	return &SummaryProvider{client: client, model: model}, nil
}

// SummarizeBehaviors analyzes recent messages and merges new evidence into the
// existing previousSummary, returning an updated behavioral profile paragraph.
// Only newMessages (since last summarization) are analyzed — not the full history.
func (s *SummaryProvider) SummarizeBehaviors(ctx context.Context, newMsgs []history.Message, previousSummary string) (string, error) {
	if len(newMsgs) == 0 {
		return previousSummary, nil
	}

	// Build compact transcript from new messages only (skip system messages)
	var sb strings.Builder
	for _, m := range newMsgs {
		if m.Role == "system" {
			continue
		}
		role := m.Role
		if role == "assistant" {
			role = "AI"
		}
		content := m.Content
		if len(content) > 300 {
			content = content[:300] + "..."
		}
		fmt.Fprintf(&sb, "[%s]: %s\n", strings.ToUpper(role), content)
	}
	if strings.TrimSpace(sb.String()) == "" {
		return previousSummary, nil
	}

	systemPrompt := `You are a behavioral analyst maintaining a running user preference profile.

Given:
1. An existing profile (may be empty on first run)
2. New conversation messages since the last update

Your job: merge new evidence into the existing profile, updating or adding insights.
Output a single updated paragraph (max 150 words) covering:
- Communication style (detailed vs concise, formal vs casual)
- Technical depth (expert, intermediate, beginner)
- Topics of interest or domain focus
- Recurring patterns or pet peeves
- Preferred response format (code, prose, bullets)

Preserve confirmed previous insights. Override only if new evidence contradicts them.
Be specific and actionable — this is injected into the AI system prompt.`

	var userContent strings.Builder
	if previousSummary != "" {
		fmt.Fprintf(&userContent, "EXISTING PROFILE:\n%s\n\n", previousSummary)
	}
	fmt.Fprintf(&userContent, "NEW MESSAGES TO ANALYZE:\n%s", sb.String())

	req := openai.ChatCompletionRequest{
		Model: s.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userContent.String()},
		},
		MaxTokens: 200,
		Stream:    true,
	}

	stream, err := s.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return "", fmt.Errorf("summary provider stream error: %w", err)
	}
	defer stream.Close()

	var result strings.Builder
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("summary provider chunk error: %w", err)
		}
		if len(resp.Choices) > 0 {
			result.WriteString(resp.Choices[0].Delta.Content)
		}
	}

	return strings.TrimSpace(result.String()), nil
}
