<div align="center">
  <h1>GopherAgent</h1>
  <p><b>Build AI Agents with YAML. Ship them in Go.</b></p>
  <p>
    <a href="https://pkg.go.dev/github.com/hung12ct/gopheragent"><img src="https://pkg.go.dev/badge/github.com/hung12ct/gopheragent.svg" alt="Go Reference"></a>
    <a href="https://github.com/hung12ct/gopheragent/actions/workflows/ci.yml"><img src="https://github.com/hung12ct/gopheragent/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
    <a href="./LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue.svg" alt="License"></a>
  </p>
</div>

Your PM writes a YAML file. Your engineer writes a Go tool. GopherAgent wires them together at runtime — no recompile, no redeploy.

```yaml
# agent.yaml — your PM creates this
agent:
  name: "Customer Support"
  system_prompt: |
    You are a customer support agent. Look up orders before answering.
    Be polite and concise. Escalate billing issues to a human.
  tools_required:
    - "lookup_order"
    - "web_search"
```

```go
// main.go — your engineer writes this once
catalog := builder.NewGlobalCatalog()
catalog.Register(&LookupOrderTool{db: db})
catalog.Register(webSearchTool)

loop, _, _, _ := builder.BuildFromYAML("agent.yaml", catalog, provider, nil)
loop.RunIteration(ctx, sessionKey, userMessage)
```

**That's it.** Change the YAML, get a different agent. No code changes.

## Installation

```bash
go get github.com/hung12ct/gopheragent
```

## The YAML Builder

Engineers register tools into a catalog; PMs wire agents with YAML.

```go
catalog := builder.NewGlobalCatalog()
catalog.Register(&CheckInventoryTool{})
catalog.Register(builtin.NewReadURLTool())
webSearch, _ := builtin.NewWebSearchTool("")
catalog.Register(webSearch)

provider, _ := llm.NewOpenAIProvider("", "gpt-4o")
loop, _, _, _ := builder.BuildFromYAML("agent.yaml", catalog, provider, nil)
resp, _ := loop.RunIteration(ctx, "session_1", "Do we have iPhone 16 in stock?")
```

More YAML agent patterns (customer support, data analyst, content writer,
multi-agent SQL hub) live in [`examples/yaml_agents`](./examples/yaml_agents).

## Built-in Tools

Import `github.com/hung12ct/gopheragent/pkg/tools/builtin`:

| Tool | Constructor | Description |
|---|---|---|
| Web search | `NewWebSearchTool(apiKey)` | Internet search via Tavily API |
| Read URL | `NewReadURLTool()` | Fetch and parse any web page to plain text; SSRF-protected |
| Show media | `NewShowMediaTool()` | Embed images or videos inline in streaming UIs |
| HTTP request | `NewHTTPRequestTool()` | Call JSON APIs and webhooks; SSRF-protected + host allowlist |
| File read | `NewFileReadTool(root)` | Read local files; path-traversal-safe root sandbox |
| Media analyze | `NewMediaAnalyzeTool(analyzer)` | Describe images or videos via any multimodal model |
| Memory set/get/delete/list | `NewMemorySetTool(store)` etc. | Agent-curated key/value facts; survives context pruning |
| Task tracking (create/update/list) | `RegisterTaskTools(registry, store)` | Structured planning scratchpad with enum status (pending/in_progress/completed) |
| Code interpreter | `NewCodeInterpreterTool()` | Execute Python or Node snippets; output-capped, timeout-bounded |
| SQL agent | `NewSQLAgentTool(db, schema, sm, provider)` | Natural language → read-only SQL; DML-proof + self-consistency |
| Generate image | `NewGenerateImageTool(apiKey, model)` | DALL-E 3 image generation; returns inline markdown embed |
| Generate video | `NewGenerateVideoTool(apiKey, model)` | Veo 2 video generation (5–8 s); inline `<video>` result |

## Writing Custom Tools

Implement the `tools.Tool` interface — one struct, five methods. Use
`tools.SchemaFor[T]()` to derive the JSON schema from a Go struct:

```go
type CheckInventoryArgs struct {
    ProductName string `json:"product_name" description:"Product to check"`
}

type CheckInventoryTool struct{ db *sql.DB }

func (t *CheckInventoryTool) Name() string        { return "check_inventory" }
func (t *CheckInventoryTool) Description() string { return "Check product stock" }
func (t *CheckInventoryTool) ParametersSchema() tools.ToolSchema {
    return tools.SchemaFor[CheckInventoryArgs]()
}
func (t *CheckInventoryTool) RequiresConfirmation() bool { return false }
func (t *CheckInventoryTool) Execute(ctx context.Context, argsJSON string) (string, error) {
    var args CheckInventoryArgs
    _ = json.Unmarshal([]byte(argsJSON), &args)
    return `{"in_stock": 250}`, nil
}
```

Supported tags: `json`, `description`, `enum`, `required`. See
[`pkg/tools/schema.go`](./pkg/tools/schema.go) for the full type list.

## Native Multimodal Input

Conversation history accepts typed `MediaPart`s — images go straight into
the LLM request on OpenAI, Anthropic, and Gemini, no base64 round-trip per
turn. See [`examples/media_chat`](./examples/media_chat).

## Production-Ready Features

All of the below are documented with full API on [pkg.go.dev](https://pkg.go.dev/github.com/hung12ct/gopheragent):

- **Tool RAG (dynamic tool selection)** — `tools.NewSelector(ctx, registry, embedder, topK)` embeds tool descriptors once at init, re-ranks per-turn by cosine similarity to the latest user message, and passes only the top-K to the LLM. Cuts prompt bloat when you have 50+ tools. OpenAI + Gemini embedders ship in `pkg/llm`.
- **Dependency-aware parallel tool scheduling** — the LLM emits `<output_of:ID.path>` inside one call's args to reference another's output; the loop parses refs, topologically orders calls into execution waves, runs each wave in parallel, and substitutes upstream JSON before the dependent call fires. One LLM round-trip instead of N. See `pkg/agent/scheduler.go`.
- **Auto-injected tool-chaining hint** — when 2+ tools are registered the loop prepends a short `<output_of:ID>` usage snippet into the system prompt at LLM-call time (never persisted). Opt out with `loop.DisableToolChainingHint = true`.
- **First-class task tracking** — `create_task` / `update_task` / `list_tasks` built-ins give the LLM a structured planning scratchpad with enum-enforced status (`pending`/`in_progress`/`completed`). Per-session isolated; wire all three via `builtin.RegisterTaskTools(registry, builtin.NewInMemoryTaskStore())`.
- **Multi-model routing** — `llm.RouterProvider` dispatches requests to different models per condition (token count, system prompt, keyword).
- **Retry + structured errors** — `agent.DefaultRetryConfig()`; `errors.Is` / `errors.As` against `ErrMaxIterations`, `ErrLLMFailure`, `LLMFailureError`, ...
- **Observability** — `loop.OnEvent(...)` for custom sinks; `telemetry.NewOTelHandler(tracer)` for OpenTelemetry; `BudgetTracker.MetricsHandler()` for Prometheus/Grafana.
- **Per-session token budget** — `agent.NewBudgetTracker(cap)` enforces spend caps as a before-LLM hook.
- **Session TTL + auto cleanup** — `sm.WithTTL(30*time.Minute).StartCleanup(ctx, 5*time.Minute)`.
- **Tool middleware** — `tools.Chain(t, WithTimeout, WithRateLimit, WithLogging)`; one-line `registry.EnableDebug(nil)` for structured tool-call logs.
- **Bounded async workers** — `agent.NewAsyncTaskManager(...).WithMaxConcurrent(8)`; 5 lifecycle tools (`start_async_task`, `check_async_task`, ...) for background work.
- **Human-in-the-loop** — `RequiresConfirmation() == true` triggers `ConfirmHITL`. See [`examples/hitl_server`](./examples/hitl_server).
- **Sub-agent streaming** — forwarded events tagged `Source="subagent:<name>"` + `ParentID`; parent UI renders nested activity timelines.
- **Conversation forking** — `sm.Fork(ctx, key, atIndex)` and `agent.ForkAtLastUser(...)` branch history safely (boundary-snapped past dangling tool calls).
- **Typed event payloads** — `ev.Payload()` returns a sealed `EventPayload` for exhaustive, compiler-checked type switches.
- **SSRF-hardened HTTP tools** — post-DNS IP check defeats rebinding; `WithAllowedHosts(...)` for per-host allowlisting.

## Supported Providers

| Provider | Constructor | Models |
|---|---|---|
| OpenAI | `llm.NewOpenAIProvider(key, model)` | gpt-4o, gpt-4o-mini, o1, ... |
| Anthropic | `llm.NewAnthropicProvider(key, model)` | claude-sonnet, claude-opus, ... |
| Google Gemini | `llm.NewGeminiProvider(key, model)` | gemini-2.5-flash, gemini-2.5-pro, ... |
| Vertex AI (Gemini) | `llm.NewVertexGeminiProvider(project, location, model)` | Vertex-hosted Gemini via ADC |
| OpenAI-compatible | `llm.NewOpenAICompatProvider(key, model, baseURL)` | Ollama, Groq, vLLM, Together, ... |

All providers auto-discover API keys from environment variables when key is `""`.

## Examples

| Example | What it shows |
|---|---|
| [`examples/demo`](./examples/demo) | Full chat UI — web research, memory sidebar, Python execution, live HITL, SSE streaming |
| [`examples/creative_studio`](./examples/creative_studio) | AI Creative Director — DALL-E 3 images + Veo 2 video clips generated inline |
| [`examples/media_chat`](./examples/media_chat) | Media Q&A — upload image/video/doc, native multimodal history, multi-turn references |
| [`examples/sse_server`](./examples/sse_server) | Minimal streaming HTTP server using Server-Sent Events |
| [`examples/hitl_server`](./examples/hitl_server) | Human-in-the-loop approvals over HTTP (async bridge) |
| [`examples/dynamic_builder`](./examples/dynamic_builder) | Load an agent from YAML at runtime |
| [`examples/multi_agent_data`](./examples/multi_agent_data) | SQL analytics hub with dynamic schema injection |
| [`examples/yaml_agents`](./examples/yaml_agents) | Multiple YAML-defined agents sharing a catalog |

```bash
cd examples/demo
echo "LLM_PROVIDER=openai\nOPENAI_API_KEY=sk-..." > .env
go run .
# open http://localhost:8888
```

## License

Apache 2.0
