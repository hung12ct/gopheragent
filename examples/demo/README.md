# Demo — Web Chat UI (SSE Streaming)

A chat web app that demonstrates **real-time agent streaming over SSE**.  
The agent is loaded from a YAML file and uses `web_search` + `read_url` to answer questions.

## What it does

- Serves a chat UI at `http://localhost:8080`
- Streams agent thoughts, tool calls, and content chunks in real time
- Agent behavior (system prompt, tools) is driven by a YAML file — no recompile needed
- Auto-loads `.env` from this folder (or `../../.env` as fallback)

## Setup

```bash
cp .env.example .env
# Fill in your keys
```

## Run

```bash
# From project root:
make example-demo

# Or directly:
go run .
```

Then open `http://localhost:8080` in your browser.

## Switch agents

Point to a different YAML file without touching any Go code:

```bash
AGENT_YAML_PATH=../yaml_agents/content_writer.yaml go run .
```

Available presets in `../yaml_agents/`:
| File | Description |
|---|---|
| `web_research_chat.yaml` | General web research (default) |
| `content_writer.yaml` | SEO content writer |
| `customer_support.yaml` | E-commerce support agent |
| `data_analyst.yaml` | SQL + business insight analyst |

## Custom tool via YAML

```go
// Register your tool once in main.go
catalog.Register(&MyCustomTool{})
```

```yaml
# Then enable it in any YAML — no code change needed
tools_required:
  - "my_custom_tool"
  - "web_search"
```
