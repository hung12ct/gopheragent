package openai

import (
	"errors"
	"os"

	"github.com/sashabaranov/go-openai"
)

// NewCompat creates a provider for any OpenAI-compatible API endpoint.
// This covers: Google Gemini (via OpenAI compat), Ollama, Groq, Together AI, vLLM, etc.
//
// Examples:
//
//	Gemini:  NewCompat("GEMINI_API_KEY", "gemini-2.0-flash", "https://generativelanguage.googleapis.com/v1beta/openai")
//	Ollama:  NewCompat("ollama", "llama3", "http://localhost:11434/v1")
//	Groq:    NewCompat("GROQ_KEY", "llama-3.3-70b-versatile", "https://api.groq.com/openai/v1")
func NewCompat(apiKey string, model string, baseURL string, opts ...Option) (*Provider, error) {
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New("API key is not set")
	}
	if baseURL == "" {
		return nil, errors.New("baseURL is required for OpenAI-compatible provider")
	}
	if model == "" {
		return nil, errors.New("model is required for OpenAI-compatible provider")
	}

	config := openai.DefaultConfig(apiKey)
	config.BaseURL = baseURL

	p := &Provider{
		client: openai.NewClientWithConfig(config),
		model:  model,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}
