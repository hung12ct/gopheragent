# Providers

Each provider lives in its own subpackage, so a binary that uses one vendor does
not statically link the other vendors' SDKs:

| Provider | Import | Constructor | Models |
|---|---|---|---|
| OpenAI | `pkg/llm/openai` | `openai.New(key, model)` | gpt-4o, gpt-4o-mini, o1, ... |
| Anthropic | `pkg/llm/anthropic` | `anthropic.New(key, model)` | claude-sonnet, claude-opus, ... |
| Google Gemini | `pkg/llm/gemini` | `gemini.New(key, model)` | gemini-2.5-flash, gemini-2.5-pro, ... |
| Vertex AI (Gemini) | `pkg/llm/gemini` | `gemini.NewVertex(project, location, model)` | Vertex-hosted Gemini via ADC |
| OpenAI-compatible | `pkg/llm/openai` | `openai.NewCompat(key, model, baseURL)` | Ollama, Groq, vLLM, Together, ... |

All providers auto-discover API keys from environment variables when key is `""`.

## Multi-model routing

`llm.RouterProvider` dispatches calls across several backing providers behind a
single `LLMProvider` interface — route by model, cost, or fallback.

## Sampling controls

Every constructor accepts sampling options for reproducible runs —
`WithTemperature(t)`, `WithTopP(p)`, and `WithSeed(n)` (OpenAI and Gemini only;
the Anthropic API has no seed parameter, so temperature 0 there is best-effort,
not bit-exact). Unset options keep each provider's defaults, so the feature is
zero-cost when unused.

## Multimodal input

Conversation history accepts typed `MediaPart`s — images go straight into the
LLM request on OpenAI, Anthropic, and Gemini, no base64 round-trip per turn. See
[`examples/media_chat`](../examples/media_chat).
