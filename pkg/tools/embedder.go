package tools

import (
	"context"
	"math"
)

// Embedder produces fixed-size vector representations of text.
//
// Implementations live in pkg/llm/openai and pkg/llm/gemini (each exports
// an Embedder backed by that vendor's embeddings API). A single
// Embed call should accept a batch and return one vector per input, in order.
// All returned vectors must share the same dimensionality.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// cosineSimilarity returns the cosine similarity of two equal-length vectors.
// Returns 0 if either vector has zero magnitude or lengths differ.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
