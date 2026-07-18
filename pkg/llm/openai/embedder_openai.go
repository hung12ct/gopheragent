package openai

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hung12ct/gopheragent/pkg/tools"
	"github.com/sashabaranov/go-openai"
)

// Embedder implements tools.Embedder using OpenAI's embeddings API.
//
// text-embedding-3-small is the default — 1536 dimensions, ~$0.02/1M tokens,
// and more than enough lexical/semantic signal for tool-description ranking.
// Pass model = "" to use the default, or a specific model id (e.g.
// openai.LargeEmbedding3 for higher fidelity at ~6x the cost).
type Embedder struct {
	client *openai.Client
	model  openai.EmbeddingModel
}

// NewEmbedder constructs an embedder. apiKey falls back to
// OPENAI_API_KEY. model defaults to text-embedding-3-small.
func NewEmbedder(apiKey string, model string) (*Embedder, error) {
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("OPENAI_API_KEY is not set in environment")
	}
	m := openai.SmallEmbedding3
	if model != "" {
		m = openai.EmbeddingModel(model)
	}
	return &Embedder{
		client: openai.NewClient(apiKey),
		model:  m,
	}, nil
}

// Embed returns one vector per input in the same order. Empty input returns
// (nil, nil).
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	resp, err := e.client.CreateEmbeddings(ctx, openai.EmbeddingRequest{
		Input: texts,
		Model: e.model,
	})
	if err != nil {
		return nil, fmt.Errorf("openai: Embedder: create embeddings: %w", err)
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("openai: Embedder: got %d embeddings for %d inputs", len(resp.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("openai: Embedder: embedding index %d out of range", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

var _ tools.Embedder = (*Embedder)(nil)
