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
// Two features default off here for the same reason the model is required:
// a compatible endpoint implements an unknown subset of the API, so the
// adapter declares nothing it cannot know.
//
//   - JSON mode is JSONModeNone. A structured-output call fails until
//     WithJSONMode says what the endpoint implements. Endpoints publishing
//     OpenAI's json_schema take JSONModeSchema; those publishing only the
//     older json_object take JSONModeObject.
//   - Image input is unclaimed until WithImageInput(true).
//
// Both feed Capabilities(), so a consumer that needs schema enforcement or
// vision can reject the provider at construction instead of at the first
// request.
//
// Examples:
//
//	Gemini:  NewCompat("GEMINI_API_KEY", "gemini-2.0-flash", "https://generativelanguage.googleapis.com/v1beta/openai",
//	             WithJSONMode(JSONModeSchema), WithImageInput(true))
//	OpenRouter: NewCompat("OPENROUTER_API_KEY", "openai/gpt-4o", "https://openrouter.ai/api/v1",
//	             WithJSONMode(JSONModeSchema), WithImageInput(true))
//	DeepSeek: NewCompat("DEEPSEEK_API_KEY", "deepseek-v4-flash", "https://api.deepseek.com/v1",
//	             WithJSONMode(JSONModeObject))
//	Ollama:  NewCompat("ollama", "llama3", "http://localhost:11434/v1", WithJSONMode(JSONModeObject))
//	Groq:    NewCompat("GROQ_KEY", "llama-3.3-70b-versatile", "https://api.groq.com/openai/v1", WithJSONMode(JSONModeObject))
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
	// Prepended, not appended: these downgrade New's OpenAI-shaped defaults
	// to "unknown endpoint", and a caller's own opts must still win.
	defaults := []Option{
		WithBaseURL(baseURL),
		WithJSONMode(JSONModeNone),
		WithImageInput(false),
	}
	return New(apiKey, model, append(defaults, opts...)...)
}
