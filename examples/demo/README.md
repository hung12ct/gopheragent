# GopherAgent — Live Demo

A full-featured chat UI that shows what GopherAgent can do out of the box:
real-time streaming, web research, session memory, Python code execution,
media display, and human-in-the-loop approval — all wired together in
**~250 lines of Go**.

```
┌─────────────────────────────────────────────────────────────┐
│  GopherAgent          web search · memory · python          │
├──────────────┬──────────────────────────────────────────────┤
│ Session      │                                              │
│ Memory       │  ⚙ Searching for "Anthropic Claude 4"…      │
│              │  ⚙ Reading anthropic.com/news…              │
│ name: Alex   │                                              │
│ interest:    │  Here's the latest from Anthropic…          │
│   Go, ML     │                                              │
│              │  > Find a chart about climate change         │
│              │  [image displayed inline]                    │
│              │                                              │
│              │  > Calculate the 1000th prime number         │
│              │  ⚠ Confirm: run Python code? [Approve]       │
│              │  Result: 7919                                │
└──────────────┴──────────────────────────────────────────────┘
```

## Capabilities

| Feature | What the agent can do |
|---|---|
| 🔍 **Web research** | Search the internet, read any URL, embed images/videos inline |
| 🧠 **Explicit memory** | Agent-curated key/value facts — survives context pruning and shared across sub-agents |
| 🐍 **Code execution** | Run Python 3 snippets for math, data transforms, and text processing |
| ✅ **HITL approval** | Code runs only after you click Approve — functional, not a mock |
| 📡 **SSE streaming** | Thoughts, tool calls, and content stream token-by-token in real time |
| 🗂️ **YAML-defined** | Swap the entire agent (prompt + tools) by changing one env variable |

## Quickstart

```bash
# 1. Copy env template and add your API key
cp ../../.env.example ../../.env
# LLM_PROVIDER=openai  (or anthropic, gemini)
# OPENAI_API_KEY=sk-...

# 2. Run
go run .

# 3. Open
open http://localhost:8888
```

The UI has six **suggested prompts** — click any to start immediately.

## Things to try

**Explicit memory** *(shines when history is long or sub-agents are involved)*
```
My name is Alex and I'm a Go developer — please remember that
```
→ Agent calls `memory_set`. The sidebar updates instantly. Ask again later:
```
What do you know about me?
```
→ Agent calls `memory_list` + `memory_get` and recalls it directly — no scanning
the full message history.

> **Session history vs. explicit memory**
> GopherAgent already stores every conversation turn in a session (backed by
> in-memory, file, or MySQL). For short conversations that's sufficient.
> Explicit memory (`memory_set/get`) is useful when:
> - History grows long enough to be **summarized/pruned** — key facts may not
>   survive the summary, but memory entries always do.
> - **Sub-agents or async workers** need shared state — they don't inherit the
>   parent's message history, but they can read the same memory store.
> - You want **structured, enumerable facts** (`memory_list`) rather than
>   asking the LLM to surface a detail buried in 80 messages.
>
> For a simple, short-lived chatbot, you don't need this tool at all.

**Web research + media**
```
Find a chart about global CO₂ emissions and show it
```
→ Agent searches, picks a URL, reads it, finds an image URL, and embeds it inline.

**Python code (with HITL)**
```
Compute the first 20 prime numbers using Python
```
→ A confirmation box appears. Click **Approve** — the code runs and the result streams back. Click **Reject** — the agent explains it was denied.

**Research + memory combo**
```
Search for the latest Go release notes and remember the version number
```
→ Agent searches the web, reads the release page, stores the version in memory.

**Math / data**
```
Use Python to calculate compound interest: $10,000 at 7% for 30 years
```

## Endpoints

| Endpoint | Purpose |
|---|---|
| `GET /` | Chat UI |
| `GET /api/chat?session_id=&message=` | SSE stream — agent turns |
| `POST /api/approve?id=&approved=true\|false` | Unblock a pending HITL decision |
| `GET /api/memory?session_id=` | JSON list of all key/value pairs for a session |

## Swap the agent via env

No code changes — just set `AGENT_YAML_PATH`:

```bash
AGENT_YAML_PATH=../yaml_agents/content_writer.yaml go run .
AGENT_YAML_PATH=../yaml_agents/data_analyst.yaml   go run .
AGENT_YAML_PATH=../yaml_agents/customer_support.yaml go run .
```

Or write your own — drop a YAML file anywhere and point the variable at it.

## Session persistence

| Variable | Backend | Survives restart? |
|---|---|---|
| `MYSQL_DSN=user:pass@tcp(host)/db` | MySQL | yes |
| `SESSION_DIR=/tmp/sessions` | JSON files | yes |
| *(neither set)* | In-memory | no |

## Add your own tools

Register a tool in `main.go` (one line), declare it in the YAML — done:

```go
// main.go
catalog.Register(&MyCRMTool{client: crmClient})
```

```yaml
# research_assistant.yaml
tools_required:
  - "web_search"
  - "memory_set"
  - "my_crm_tool"   # ← your tool is now available to the agent
```

The agent decides when and how to use it — no prompt engineering required.
