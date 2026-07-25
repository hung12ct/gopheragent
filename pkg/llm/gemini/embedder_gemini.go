package gemini

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hung12ct/gopheragent/pkg/tools"
	"google.golang.org/genai"
)

// Embedder implements tools.Embedder using Google Gemini embeddings.
//
// Default model is text-embedding-004 (768 dims, retrieval-tuned). The SDK
// batches automatically; large inputs should still be chunked by the caller
// if they approach the per-request token limit.
type Embedder struct {
	client *genai.Client
	model  string
}

// NewEmbedder constructs an embedder. apiKey falls back to
// GEMINI_API_KEY. model defaults to "text-embedding-004".
func NewEmbedder(apiKey string, model string) (*Embedder, error) {
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
		return nil, fmt.Errorf("gemini: NewEmbedder: %w", err)
	}
	return &Embedder{client: client, model: model}, nil
}

// Embed returns one vector per input in the same order.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
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
		return nil, fmt.Errorf("gemini: Embedder: embed content: %w", classifyErr(err))
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("gemini: Embedder: got %d embeddings for %d inputs", len(resp.Embeddings), len(texts))
	}
	out := make([][]float32, len(texts))
	for i, emb := range resp.Embeddings {
		if emb == nil {
			return nil, fmt.Errorf("gemini: Embedder: nil embedding at index %d", i)
		}
		out[i] = emb.Values
	}
	return out, nil
}

var _ tools.Embedder = (*Embedder)(nil)
