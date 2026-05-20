<div align="center">
  <h1>GopherAgent — Go / Golang Agent Framework</h1>
  <p><b>Build production LLM agents with YAML. Ship them in Go.</b></p>
  <p>
    <a href="https://pkg.go.dev/github.com/hung12ct/gopheragent"><img src="https://pkg.go.dev/badge/github.com/hung12ct/gopheragent.svg" alt="Go Reference"></a>
    <a href="https://github.com/hung12ct/gopheragent/actions/workflows/ci.yml"><img src="https://github.com/hung12ct/gopheragent/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
    <a href="./LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue.svg" alt="License"></a>
  </p>
</div>

**GopherAgent** is a **Golang multi-agent LLM framework** — deterministic ReAct loops, parallel tool execution, streaming, sub-agents, and multi-model routing. Your PM writes a YAML file. Your engineer registers a Go tool. GopherAgent wires them together at runtime — no recompile, no redeploy.

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

## Permission Flow

When a tool returns `RequiresConfirmation() = true`, the loop denies the
call **unless one of**:

- A `loop.ConfirmHITL` hook returns `true` (human approves), or
- A `loop.Permissions` rule returns `PermissionAllow` (policy auto-approves).

With neither, the call is denied and the model receives a directive
"permission gate" message telling it to ask the user for approval.

For autonomous agents (no human reviewer), pre-approve trusted tools:

```go
loop.Permissions = agent.NewPermissionRuleSet().
    Allow("call_sql_agent").     // bypass HITL for this tool
    Deny("dangerous_tool")       // deny-over-allow ordering
```

Or opt the tool out of confirmation entirely at the source:

```go
sqlTool := builtin.NewSQLAgentTool(db, "", sm, provider).
    WithRequiresConfirmation(false)
```

## Native Multimodal Input

Conversation history accepts typed `MediaPart`s — images go straight into
the LLM request on OpenAI, Anthropic, and Gemini, no base64 round-trip per
turn. See [`examples/media_chat`](./examples/media_chat).

## Production-Ready Features

Full API on [pkg.go.dev](https://pkg.go.dev/github.com/hung12ct/gopheragent).

**Runtime**
- Streaming (SSE), HITL approvals, plan mode, self-critique (Reflect)
- Dependency-aware parallel tool scheduling with `<output_of:ID>` refs
- Sub-agent streaming, conversation forking, first-class task tracking

**Cost & performance**
- Structured output / JSON mode across OpenAI, Anthropic, Gemini
- Anthropic prompt-cache hints, speculative tool execution
- Per-session token budget, thinking budget, tool RAG (50+ tools)
- Multi-model routing (`llm.RouterProvider`)

**Reliability**
- Exponential-backoff retry, structured errors (`errors.Is` / `errors.As`)
- Structured tool-error hints for better LLM recovery
- Session TTL + auto cleanup, SSRF-hardened HTTP tools

**Ops**
- OpenTelemetry tracing, Prometheus via `BudgetTracker.MetricsHandler()`
- Custom event sinks (`OnEvent`), tool middleware chain

**Extensibility**
- YAML-driven agents (file or `//go:embed`) + knowledge base injection
- Permission DSL (`Allow` / `Deny` glob patterns)
- Typed event payloads, bounded async workers
- Cross-session memory (`pkg/memory`): post-session `Consolidator` distills
  transcripts into notes that the loader prepends to future sessions —
  agents learn between conversations without retraining

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
| [`examples/hitl_server`](./examples/hitl_server) | Human-in-the-loop approvals over HTTP (async bridge) |
| [`examples/yaml_agents`](./examples/yaml_agents) | Multiple YAML-defined agents sharing a catalog |

```bash
cd examples/demo
echo "LLM_PROVIDER=openai\nOPENAI_API_KEY=sk-..." > .env
go run .
# open http://localhost:8888
```

## License

Apache 2.0
