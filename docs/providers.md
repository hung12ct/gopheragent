# Providers

Each provider lives in its own subpackage, so a binary that uses one vendor does
not statically link the other vendors' SDKs:

| Provider | Import | Constructor | Models |
|---|---|---|---|
| OpenAI | `pkg/llm/openai` | `openai.New(key, model)` | gpt-4o, gpt-4o-mini, o1, ... |
| Anthropic | `pkg/llm/anthropic` | `anthropic.New(key, model)` | claude-sonnet, claude-opus, ... |
| Google Gemini | `pkg/llm/gemini` | `gemini.New(key, model)` | gemini-2.5-flash, gemini-2.5-pro, ... |
| Vertex AI (Gemini) | `pkg/llm/gemini` | `gemini.NewVertex(project, location, model)` | Vertex-hosted Gemini via ADC |
| OpenAI-compatible | `pkg/llm/openai` | `openai.NewCompat(key, model, baseURL)` | OpenRouter, Ollama, Groq, vLLM, Together, ... |

All providers auto-discover API keys from environment variables when key is `""`.

OpenRouter uses the existing OpenAI-compatible provider; it does not need a
separate adapter:

```go
p, err := openai.NewCompat(key, "openai/gpt-4o", "https://openrouter.ai/api/v1",
	openai.WithHTTPHeader("HTTP-Referer", "https://example.com"),
	openai.WithHTTPHeader("X-Title", "my-agent"),
)
```

Compatible base URLs must be absolute HTTP(S) URLs without embedded
credentials, query parameters, or fragments. Use HTTPS for remote gateways;
plain HTTP remains available for local Ollama/vLLM development.

### Declare what the endpoint implements

"OpenAI-compatible" covers the endpoint, the request/response shape, and the
SDK — not every extension built on top of them. `NewCompat` therefore claims
nothing beyond plain chat and tool calling, in the same spirit as requiring an
explicit model: the adapter cannot infer the rest from a base URL.

Two declarations matter:

| Option | Effect |
|---|---|
| `WithJSONMode(JSONModeSchema)` | `response_format {type:"json_schema"}` with the schema inline, enforced server-side. What `api.openai.com` implements. |
| `WithJSONMode(JSONModeObject)` | `response_format {type:"json_object"}` plus the schema appended to the prompt, for endpoints publishing only the older format. |
| `WithImageInput(true)` | Declares that the endpoint accepts image parts on user messages. |

```go
// Endpoint publishing OpenAI's json_schema:
p, _ := openai.NewCompat(key, "openai/gpt-4o", "https://openrouter.ai/api/v1",
	openai.WithJSONMode(openai.JSONModeSchema), openai.WithImageInput(true))

// Endpoint publishing only json_object:
p, _ := openai.NewCompat(key, "deepseek-v4-flash", "https://api.deepseek.com/v1",
	openai.WithJSONMode(openai.JSONModeObject))
```

Without a declaration the JSON mode is `JSONModeNone`, and a structured-output
call fails with an error naming the option instead of sending a
`response_format` the endpoint may reject. Check the endpoint's own docs for
which formats it publishes — the two are not interchangeable, and sending the
wrong one is a 400 on every structured call.

`JSONModeObject` guarantees only that the reply parses as JSON. Schema
conformance degrades from a server-side constraint to a prompt instruction, so
validate the result and be ready to retry; `Strict` becomes a request rather
than a rule. This is the same trade the Anthropic adapter avoids by
synthesizing a tool — an option `json_object` endpoints do not offer.

### Point the non-chat clients at the same endpoint

`NewCompat` configures the **chat provider only**. The embedder, vision
analyzer, and summary provider take the same transport options, and without
them they call `api.openai.com` — on a deliberately local deployment that is an
unexpected egress and a key you may not hold. Pass `WithBaseURL` to each:

```go
base := openai.WithBaseURL("http://localhost:11434/v1")

chat, _ := openai.New(key, "llama3", base, openai.WithTemperature(0.2))
emb,  _ := openai.NewEmbedder(key, "nomic-embed-text", base)
vis,  _ := openai.NewVisionAnalyzer(key, "llava", base)
sum,  _ := openai.NewSummaryProvider(key, "llama3", base)
```

`WithBaseURL` and `WithHTTPHeader` work on all four. The sampling options
(`WithTemperature`, `WithTopP`, `WithSeed`) apply to the chat provider only,
and passing one to `NewEmbedder` is a compile error rather than a setting that
silently does nothing.

## Provider capabilities

`agent.CapabilityProvider` lets a consumer that requires image input or
structured output reject an unsuitable provider at construction, instead of
discovering the gap from a confident, wrong answer. `openai.New`, Anthropic,
and Gemini report both; `openai.NewCompat` reports what the caller declared
(see above); `llmfake.ScriptedProvider` reports neither, since it replays a
script without ever reading a message's media parts.

```go
if c, ok := provider.(agent.CapabilityProvider); ok && !c.Capabilities().ImageInput {
	return fmt.Errorf("judge requires a multimodal provider")
}
```

Two rules keep that check meaningful:

- **Absence means unknown, not false.** A provider that does not implement the
  interface makes no claim; the caller decides how strict to be.
- **Decorators forward.** `otelllm.NewProvider` passes the wrapped provider's
  report through, and does not implement the interface when the wrapped
  provider doesn't — so enabling tracing never erases the signal.
  `llm.RouterProvider` reports the intersection over its fallback and every
  route, because the route is chosen from the conversation and is unknown
  until the call runs; one undeclared member collapses the report to "supports
  nothing".

It does not replace model discovery: gateways such as OpenRouter expose
text-only and multimodal models through the same adapter, so applications must
still verify the selected model's live metadata when correctness or spend
depends on it.

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
