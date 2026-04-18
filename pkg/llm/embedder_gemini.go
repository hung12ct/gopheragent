package llm

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hung12ct/gopheragent/pkg/tools"
	"google.golang.org/genai"
)

// GeminiEmbedder implements tools.Embedder using Google Gemini embeddings.
//
// Default model is text-embedding-004 (768 dims, retrieval-tuned). The SDK
// batches automatically; large inputs should still be chunked by the caller
// if they approach the per-request token limit.
type GeminiEmbedder struct {
	client *genai.Client
	model  string
}

// NewGeminiEmbedder constructs an embedder. apiKey falls back to
// GEMINI_API_KEY. model defaults to "text-embedding-004".
func NewGeminiEmbedder(apiKey string, model string) (*GeminiEmbedder, error) {
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("GEMINI_API_KEY is not set in environment")
	}
	if model == "" {
		model = "text-embedding-004"
	}
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{APIKey: apiKey})
	if err != nil {
		return nil, fmt.Errorf("llm: NewGeminiEmbedder: %w", err)
	}
	return &GeminiEmbedder{client: client, model: model}, nil
}

// Embed returns one vector per input in the same order.
func (e *GeminiEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	contents := make([]*genai.Content, len(texts))
	for i, t := range texts {
		contents[i] = &genai.Content{Parts: []*genai.Part{{Text: t}}}
	}
	resp, err := e.client.Models.EmbedContent(ctx, e.model, contents, &genai.EmbedContentConfig{
		TaskType: "RETRIEVAL_DOCUMENT",
	})
	if err != nil {
		return nil, fmt.Errorf("llm: GeminiEmbedder: embed content: %w", err)
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("llm: GeminiEmbedder: got %d embeddings for %d inputs", len(resp.Embeddings), len(texts))
	}
	out := make([][]float32, len(texts))
	for i, emb := range resp.Embeddings {
		if emb == nil {
			return nil, fmt.Errorf("llm: GeminiEmbedder: nil embedding at index %d", i)
		}
		out[i] = emb.Values
	}
	return out, nil
}

var _ tools.Embedder = (*GeminiEmbedder)(nil)
