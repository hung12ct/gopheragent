<div align="center">
  <h1>GopherAgent</h1>
  <p><b>Build AI Agents with YAML. Ship them in Go.</b></p>
  <p>
    <a href="https://pkg.go.dev/github.com/hung12ct/gopheragent"><img src="https://pkg.go.dev/badge/github.com/hung12ct/gopheragent.svg" alt="Go Reference"></a>
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

## The YAML Builder — Let Business Build Agents

The core idea: **engineers build tools, business builds agents**.

Engineers register tools into a Global Catalog:

```go
catalog := builder.NewGlobalCatalog()
catalog.Register(&CheckInventoryTool{})
catalog.Register(&LookupOrderTool{db: db})
catalog.Register(builtin.NewReadURLTool())
webSearch, _ := builtin.NewWebSearchTool("")
catalog.Register(webSearch)
```

Business/PMs create agents by writing YAML — no Go knowledge needed:

```yaml
agent:
  name: "Sales Assistant"
  system_prompt: |
    You are a sales assistant. Before answering questions about shipping,
    always check the inventory first!
  max_iterations: 10
  tools_required:
    - "check_inventory"   # mapped to Go struct automatically
    - "web_search"
```

Load and run:

```go
provider, _ := llm.NewOpenAIProvider("", "gpt-4o")
loop, _, _, _ := builder.BuildFromYAML("agent.yaml", catalog, provider, nil)

resp, _ := loop.RunIteration(ctx, "session_1", "Do we have iPhone 16 in stock?")
fmt.Println(resp)
```

### Example YAML Agents

<details>
<summary><b>Customer Support Agent</b></summary>

```yaml
agent:
  name: "Customer Support"
  system_prompt: |
    You are a friendly customer support agent for an e-commerce platform.
    
    RULES:
    - Always look up the order before answering shipping/return questions.
    - If the customer is angry, acknowledge their frustration first.
    - For billing disputes, say: "Let me escalate this to our billing team."
    - Never make up order statuses — use the lookup tool.
  max_iterations: 8
  tools_required:
    - "lookup_order"
    - "web_search"
```
</details>

<details>
<summary><b>Data Analyst Agent</b></summary>

```yaml
agent:
  name: "Data Analyst"
  system_prompt: |
    You are a senior data analyst. Translate business questions into SQL queries.
    
    WORKFLOW:
    1. Clarify the question if ambiguous.
    2. Use call_sql_agent to query the database.
    3. Summarize results in a clear markdown table.
    4. Add business insights — don't just dump raw numbers.
    
    RULES:
    - Never SELECT * — pick only needed columns.
    - Always LIMIT to 100 rows maximum.
    - If the query returns no results, suggest alternative queries.
  max_iterations: 12
  tools_required:
    - "call_sql_agent"
    - "web_search"
```
</details>

<details>
<summary><b>Content Writer Agent</b></summary>

```yaml
agent:
  name: "Content Writer"
  system_prompt: |
    You are an SEO-focused content writer.
    
    WORKFLOW:
    1. Research the topic using web_search and read_url.
    2. Read at least 2 source articles with read_url before writing.
    3. Write original content — never copy-paste from sources.
    4. Include relevant keywords naturally.
    5. Output in markdown with proper headings.
    
    STYLE: Professional but approachable. Short paragraphs. Use examples.
  max_iterations: 15
  tools_required:
    - "web_search"
    - "read_url"
```
</details>

<details>
<summary><b>Multi-Agent SQL Analytics Hub</b></summary>

```yaml
agent:
  name: "SQL Analytics Hub"
  system_prompt: |
    You are an AI Data Scientist. Classify requests and delegate to specialists.
    
    WORKFLOW:
    1. If data retrieval needed → call_sql_agent
    2. If external context needed → web_search
    3. If deep analysis needed → call_analytics_agent
    4. Summarize final results in markdown with tables.
  max_iterations: 15
  tools_required:
    - "call_sql_agent"
    - "call_analytics_agent"
    - "web_search"
```
</details>

## Built-in Tools

Import `github.com/hung12ct/gopheragent/pkg/tools/builtin`:

| Tool | Description |
|---|---|
| `NewWebSearchTool(apiKey)` | Internet search via Tavily API |
| `NewSQLAgentTool(db, schema, sm, provider)` | Natural language to SQL with Anti-DML protection |
| `NewReadURLTool()` | Read & summarize any URL (built-in HTML parser) |

## Writing Custom Tools

Implement the `tools.Tool` interface — one struct, five methods:

```go
type CheckInventoryTool struct{ db *sql.DB }

func (t *CheckInventoryTool) Name() string        { return "check_inventory" }
func (t *CheckInventoryTool) Description() string  { return "Check product stock in warehouse" }
func (t *CheckInventoryTool) ParametersSchema() tools.ToolSchema {
    return tools.ToolSchema{
        Type: "object",
        Properties: map[string]interface{}{
            "product_name": map[string]interface{}{"type": "string", "description": "Product to check"},
        },
        Required: []string{"product_name"},
    }
}
func (t *CheckInventoryTool) RequiresConfirmation() bool { return false }
func (t *CheckInventoryTool) Execute(ctx context.Context, argsJSON string) (string, error) {
    // your logic here
    return `{"in_stock": 250, "warehouse": "HCM-01"}`, nil
}
```

Register it once → every YAML agent can use it:
```go
catalog.Register(&CheckInventoryTool{db: db})
```

## Production Patterns

### Multi-Model Router

Route cheap tasks to fast models, complex tasks to powerful ones:

```go
fast, _ := llm.NewOpenAIProvider("", "gpt-4o-mini")
powerful, _ := llm.NewOpenAIProvider("", "gpt-4o")
claude, _ := llm.NewAnthropicProvider("", "")

router := llm.NewRouterProvider(powerful).
    AddRoute(llm.IfSystemPromptContains("summarizer"), fast).
    AddRoute(llm.IfTokensUnder(300), fast).
    AddRoute(llm.IfLastMessageContains("sql", "query"), claude)

loop := agent.NewAgentLoop(sessions, registry, router)
```

### Session TTL + Auto Cleanup

```go
sm := history.NewInMemSessionManager("You are an assistant.").
    WithTTL(30 * time.Minute).
    StartCleanup(ctx, 5 * time.Minute)
```

### Observability + Retry

```go
loop.Retry = agent.DefaultRetryConfig()
loop.OnEvent(telemetry.NewOTelHandler(tracer))
loop.OnEvent(func(ctx context.Context, sessionKey string, ev agent.StreamEvent) {
    // your metrics/logging here
})
```

### Tool Middleware

```go
import "github.com/hung12ct/gopheragent/pkg/tools"

reg.Register(tools.Chain(myTool,
    tools.WithTimeout(10 * time.Second),
    tools.WithRateLimit(5),
    tools.WithLogging(slog.Default()),
))
```

### Structured Error Handling

```go
_, err := loop.RunIteration(ctx, key, input)

if errors.Is(err, agent.ErrMaxIterations) { /* hit iteration cap */ }
if errors.Is(err, agent.ErrLLMFailure)    { /* provider error */ }
if errors.Is(err, agent.ErrToolNotFound)  { /* missing tool */ }

var lfe *agent.LLMFailureError
if errors.As(err, &lfe) {
    log.Println("provider error:", lfe.Cause)
}
```

## Supported Providers

| Provider | Constructor | Models |
|---|---|---|
| OpenAI | `llm.NewOpenAIProvider(key, model)` | gpt-4o, gpt-4o-mini, o1, ... |
| Anthropic | `llm.NewAnthropicProvider(key, model)` | claude-sonnet, claude-opus, ... |
| Google Gemini | `llm.NewGeminiProvider(key, model)` | gemini-2.5-flash, gemini-2.5-pro, ... |
| OpenAI-compatible | `llm.NewOpenAICompatProvider(key, model, baseURL)` | Ollama, Groq, vLLM, Together, ... |

All providers auto-discover API keys from environment variables when key is `""`.

## Examples

See `examples/` — each folder has its own `README.md` and `.env.example`.

## License

Apache 2.0
