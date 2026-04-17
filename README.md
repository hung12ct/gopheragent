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

Implement the `tools.Tool` interface — one struct, five methods. Use
`tools.SchemaFor[T]()` to derive the JSON schema from a Go struct instead
of hand-writing `map[string]any` literals:

```go
type CheckInventoryArgs struct {
    ProductName string `json:"product_name" description:"Product to check"`
}

type CheckInventoryTool struct{ db *sql.DB }

func (t *CheckInventoryTool) Name() string               { return "check_inventory" }
func (t *CheckInventoryTool) Description() string        { return "Check product stock in warehouse" }
func (t *CheckInventoryTool) ParametersSchema() tools.ToolSchema {
    return tools.SchemaFor[CheckInventoryArgs]()
}
func (t *CheckInventoryTool) RequiresConfirmation() bool { return false }
func (t *CheckInventoryTool) Execute(ctx context.Context, argsJSON string) (string, error) {
    var args CheckInventoryArgs
    if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
        return "", err
    }
    // your logic here
    return `{"in_stock": 250, "warehouse": "HCM-01"}`, nil
}
```

Supported tags: `json:"name,omitempty"`, `description:"..."`, `enum:"a,b,c"`,
`required:"true"`/`"false"`. Pointers and `omitempty` fields default to
optional. See `pkg/tools/schema.go` for the full list of supported types.

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

### Debug Mode — Log All Tool Calls

One line to enable structured logging for every tool in a registry:

```go
registry := tools.NewRegistry()
registry.Register(myTool)
registry.Register(sqlTool)

if debug {
    registry.EnableDebug(nil) // nil = use slog.Default()
}
```

Every tool call will log name, args, result size, and errors:

```
INFO tool call  tool=call_sql_agent args={"query":"top 5 customers"}
INFO tool ok    tool=call_sql_agent result_bytes=312
```

Pass a custom `*slog.Logger` for JSON output or custom sinks:

```go
registry.EnableDebug(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
```

### Text-to-SQL Agent (`SQLAgentTool`)

A sub-agent that converts natural-language questions into read-only SQL.
Implements the techniques from Google's [*Techniques for improving
text-to-SQL*](https://cloud.google.com/blog/products/databases/techniques-for-improving-text-to-sql):
structured schema grounding, few-shot examples, business glossary,
comment-aware validation, LIMIT injection, per-query timeouts, and optional
self-consistency.

```go
schema := builtin.Schema{
    Tables: []builtin.TableSchema{{
        Name: "customers", Description: "Paying customers.",
        Columns: []builtin.ColumnSchema{
            {Name: "id", Type: "INT"},
            {Name: "status", Type: "VARCHAR(16)",
                Examples: []string{"ACTIVE", "CHURNED"}},
        },
        PrimaryKey: []string{"id"},
    }},
}

sqlTool := builtin.NewSQLAgentTool(db, "", sessions, provider).
    WithSchema(schema).
    WithBusinessRules(
        "'active' customers have status='ACTIVE' — never 'active'.",
        "Revenue excludes refunds; always JOIN refunds and subtract.",
    ).
    WithExamples(builtin.SQLExample{
        Question: "How many active customers?",
        SQL:      "SELECT COUNT(*) FROM customers WHERE status = 'ACTIVE'",
    }).
    WithMaxRows(1000).                   // auto-append LIMIT 1000 when absent
    WithQueryTimeout(5 * time.Second).   // per-query context deadline
    WithSelfConsistency(3).              // run 3 in parallel, vote on result
    OnSQL(func(ctx context.Context, ev builtin.SQLQueryEvent) {
        slog.InfoContext(ctx, "sql_agent.query",
            "session", ev.SessionKey, "sql", ev.Query, "err", ev.Error,
        )
    })
```

**Safety guarantees on every executed statement:**

- Multi-statement input is rejected (`SELECT 1; DROP TABLE x` → error).
- Comments are stripped before classification — `/* SELECT */ DROP` is
  correctly flagged as DROP.
- Only `SELECT`, `WITH`, `EXPLAIN`, `SHOW`, `DESCRIBE` may start a statement.
- `WithMaxRows(n)` appends `LIMIT n` when missing, plus a `2×n` defensive
  hard cap on scanned rows.
- `execute_sql` returns a structured envelope (`{sql, columns, rows,
  row_count, execution_ms, truncated, error}`) so the LLM can make informed
  retry decisions.

### Per-Session Token Budget

Cap spend per conversation. `BudgetTracker` plugs into the loop as an event
handler (to accumulate usage) and a before-LLM hook (to deny new calls once
the cap is hit):

```go
bt := agent.NewBudgetTracker(100_000) // 100k tokens per session

loop.OnEvent(bt.Handler())
loop.BeforeLLMHooks = append(loop.BeforeLLMHooks, bt.Guard())

// Inspect or reset at any time:
used := bt.Usage("session-123")
bt.Reset("session-123")
```

Every provider emits a `usage` StreamEvent after each LLM call carrying
prompt / completion / total tokens — use the same handler for cost tracking,
dashboards, or per-tenant billing.

### Bounded Async Workers

`AsyncTaskManager` spawns a goroutine per background task. Cap concurrency so
a burst of `start_async_task` calls cannot exhaust the process:

```go
mgr := agent.NewAsyncTaskManager(sessions, registry, provider).
    WithMaxConcurrent(8)
```

`StartTask` returns `agent: async task cap reached (8 in flight)` when the
cap is saturated — the caller decides whether to retry, queue externally, or
surface to the user. Cancelling or completing any in-flight task frees a slot.

### Human-In-The-Loop

Tools that declare `RequiresConfirmation() == true` trigger `ConfirmHITL`
before execution. See [`examples/hitl_server`](./examples/hitl_server) for a
reference bridge between the synchronous callback and an async HTTP approval
endpoint.

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
| Vertex AI (Gemini) | `llm.NewVertexGeminiProvider(project, location, model)` | Vertex-hosted Gemini via ADC |
| OpenAI-compatible | `llm.NewOpenAICompatProvider(key, model, baseURL)` | Ollama, Groq, vLLM, Together, ... |

All providers auto-discover API keys from environment variables when key is `""`.

## Examples

See `examples/` — each folder has its own `README.md` and `.env.example`.

| Example | What it shows |
|---|---|
| [`examples/sse_server`](./examples/sse_server) | 5-minute streaming HTTP server using Server-Sent Events |
| [`examples/hitl_server`](./examples/hitl_server) | Human-in-the-loop approvals over HTTP (async bridge) |
| [`examples/dynamic_builder`](./examples/dynamic_builder) | Load an agent from YAML at runtime |
| [`examples/multi_agent_data`](./examples/multi_agent_data) | SQL analytics hub with dynamic schema injection |
| [`examples/yaml_agents`](./examples/yaml_agents) | Multiple YAML-defined agents sharing a catalog |

## License

Apache 2.0
