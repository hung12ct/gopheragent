package llm

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"google.golang.org/genai"
)

// GeminiMediaAnalyzer uses Gemini's multimodal API to analyze images and videos.
// It accepts data URIs ("data:<mime>;base64,<data>") and passes them as inline
// blobs, so it works for any MIME type Gemini supports — including video/mp4,
// video/webm, video/quicktime, image/*, etc.
type GeminiMediaAnalyzer struct {
	client *genai.Client
	model  string
}

// NewGeminiMediaAnalyzer builds an analyzer. apiKey defaults to GEMINI_API_KEY;
// model defaults to "gemini-2.5-flash".
func NewGeminiMediaAnalyzer(apiKey, model string) (*GeminiMediaAnalyzer, error) {
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("llm: GEMINI_API_KEY not set")
	}
	if model == "" {
		model = "gemini-2.5-flash"
	}
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey: apiKey,
	})
	if err != nil {
		return nil, fmt.Errorf("llm: gemini media: %w", err)
	}
	return &GeminiMediaAnalyzer{client: client, model: model}, nil
}

// Analyze sends the media data URI and prompt to Gemini and returns the response.
// media must be a data URI of the form "data:<mime>;base64,<data>".
func (a *GeminiMediaAnalyzer) Analyze(ctx context.Context, media, prompt string) (string, error) {
	mimeType, raw, err := parseDataURI(media)
	if err != nil {
		return "", fmt.Errorf("llm: gemini media: %w", err)
	}

	contents := []*genai.Content{
		{
			Parts: []*genai.Part{
				{Text: prompt},
				{InlineData: &genai.Blob{MIMEType: mimeType, Data: raw}},
			},
		},
	}

	resp, err := a.client.Models.GenerateContent(ctx, a.model, contents, nil)
	if err != nil {
		return "", fmt.Errorf("llm: gemini media: %w", err)
	}
	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return "", fmt.Errorf("llm: gemini media: no content returned")
	}
	var sb strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		if p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// parseDataURI splits "data:<mime>;base64,<data>" into mimeType and raw bytes.
func parseDataURI(uri string) (mimeType string, data []byte, err error) {
	if !strings.HasPrefix(uri, "data:") {
		return "", nil, fmt.Errorf("expected data URI")
	}
	rest := uri[5:]
	semi := strings.IndexByte(rest, ';')
	if semi < 0 {
		return "", nil, fmt.Errorf("malformed data URI: missing ';'")
	}
	mimeType = rest[:semi]
	encoded := strings.TrimPrefix(rest[semi+1:], "base64,")
	data, err = base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", nil, fmt.Errorf("decode base64: %w", err)
	}
	return mimeType, data, nil
}
