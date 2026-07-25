# Tools

## Built-in tools

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

## Writing custom tools

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

Supported struct tags: `json`, `description`, `enum`, `required`. See
[`pkg/tools/schema.go`](../pkg/tools/schema.go) for the full type list.

## Middleware

Wrap tools with cross-cutting behaviour via `tools.Chain(tool, mws...)`:
logging (`WithLogging`), timing (`WithTiming`), timeouts (`WithTimeout`),
rate limiting (`WithRateLimit`), schema validation (`WithSchemaValidation`), and
OpenTelemetry spans/metrics (`oteltools.Instrument`, see
[observability](observability.md)). To apply middleware to every tool a YAML
agent uses, register it once on the catalog with `catalog.Use(mw...)`.
