package llm

import (
	"errors"
	"os"

	"github.com/sashabaranov/go-openai"
)

// NewOpenAICompatProvider creates a provider for any OpenAI-compatible API endpoint.
// This covers: Google Gemini (via OpenAI compat), Ollama, Groq, Together AI, vLLM, etc.
//
// Examples:
//
//	Gemini:  NewOpenAICompatProvider("GEMINI_API_KEY", "gemini-2.0-flash", "https://generativelanguage.googleapis.com/v1beta/openai")
//	Ollama:  NewOpenAICompatProvider("ollama", "llama3", "http://localhost:11434/v1")
//	Groq:    NewOpenAICompatProvider("GROQ_KEY", "llama-3.3-70b-versatile", "https://api.groq.com/openai/v1")
func NewOpenAICompatProvider(apiKey string, model string, baseURL string) (*OpenAIProvider, error) {
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

	return &OpenAIProvider{
		client: openai.NewClientWithConfig(config),
		model:  model,
	}, nil
}
