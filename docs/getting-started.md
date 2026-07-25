# Getting started

## Install

```bash
go get github.com/hung12ct/gopheragent
```

## The YAML builder

Engineers register tools into a catalog; PMs wire agents with YAML. Change the
YAML, get a different agent — no recompile.

```go
catalog := builder.NewGlobalCatalog()
catalog.Register(&CheckInventoryTool{})
catalog.Register(builtin.NewReadURLTool())
webSearch, _ := builtin.NewWebSearchTool("")
catalog.Register(webSearch)

provider, _ := openai.New("", "gpt-4o") // pkg/llm/openai
loop, _, _, _ := builder.BuildFromYAML("agent.yaml", catalog, provider, nil)
resp, _ := loop.RunIteration(ctx, "session_1", "Do we have iPhone 16 in stock?")
```

```yaml
# agent.yaml
agent:
  name: "Inventory Assistant"
  system_prompt: "You help check product stock. Be concise."
  tools_required:
    - "check_inventory"
    - "read_url"
    - "web_search"
```

More YAML agent patterns (customer support, data analyst, content writer,
multi-agent SQL hub) live in [`examples/yaml_agents`](../examples/yaml_agents).

## Persistent sessions

`BuildFromYAML` uses an in-memory session store. For sessions that survive
restarts, use `BuildFromYAMLWithSession` with a File- or MySQL-backed manager:

```go
sm, _ := history.NewFileSessionManager("./sessions", cfg.Agent.SystemPrompt)
loop, _, _, _ := builder.BuildFromYAMLWithSession("agent.yaml", catalog, provider, nil, sm)
```

Next: [write custom tools](tools.md), [gate dangerous tools](permissions.md), or
[add observability](observability.md).
