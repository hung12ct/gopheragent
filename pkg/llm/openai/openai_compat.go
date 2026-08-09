package openai

import (
	"errors"
	"fmt"
)

// NewCompat creates a provider for any OpenAI-compatible API endpoint.
// This covers: OpenRouter, Google Gemini (via OpenAI compat), Ollama, Groq,
// Together AI, vLLM, etc. baseURL must be an absolute HTTP(S) URL without
// embedded credentials, a query, or a fragment. Prefer HTTPS except for local
// development endpoints.
//
// It is New plus WithBaseURL, with baseURL required rather than optional and
// model required rather than defaulted — a compatible endpoint has no
// sensible default model. Note that this configures the chat provider only:
// point NewEmbedder, NewVisionAnalyzer, and NewSummaryProvider at the same
// endpoint with WithBaseURL, or they will call api.openai.com.
//
// Examples:
//
//	Gemini:  NewCompat("GEMINI_API_KEY", "gemini-2.0-flash", "https://generativelanguage.googleapis.com/v1beta/openai")
//	OpenRouter: NewCompat("OPENROUTER_API_KEY", "openai/gpt-4o", "https://openrouter.ai/api/v1")
//	Ollama:  NewCompat("ollama", "llama3", "http://localhost:11434/v1")
//	Groq:    NewCompat("GROQ_KEY", "llama-3.3-70b-versatile", "https://api.groq.com/openai/v1")
func NewCompat(apiKey string, model string, baseURL string, opts ...Option) (*Provider, error) {
	if resolveAPIKey(apiKey) == "" {
		return nil, errors.New("openai: NewCompat: API key is not set")
	}
	if model == "" {
		return nil, errors.New("openai: NewCompat: model is required")
	}
	// Validated here rather than left to New so the error names NewCompat,
	// which is the call the caller actually made.
	if _, err := validateBaseURL(baseURL); err != nil {
		return nil, fmt.Errorf("openai: NewCompat: %w", err)
	}
	return New(apiKey, model, append([]Option{WithBaseURL(baseURL)}, opts...)...)
}
