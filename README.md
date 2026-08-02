<div align="center">
  <h1>GopherAgent — Go / Golang Agent Framework</h1>
  <p><b>Build production LLM agents with YAML. Ship them in Go.</b></p>
  <p>
    <a href="https://pkg.go.dev/github.com/hung12ct/gopheragent"><img src="https://pkg.go.dev/badge/github.com/hung12ct/gopheragent.svg" alt="Go Reference"></a>
    <a href="https://github.com/hung12ct/gopheragent/actions/workflows/ci.yml"><img src="https://github.com/hung12ct/gopheragent/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
    <a href="./LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue.svg" alt="License"></a>
  </p>
</div>

**GopherAgent** is a **Golang multi-agent LLM framework** — deterministic ReAct
loops, parallel tool execution, streaming, sub-agents, and multi-model routing.
Your PM writes a YAML file. Your engineer registers a Go tool. GopherAgent wires
them together at runtime — no recompile, no redeploy.

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

## Install

```bash
go get github.com/hung12ct/gopheragent
```

## What you get

- **YAML-defined agents** — file or `//go:embed`, with knowledge-base injection.
- **Skills with progressive disclosure** — an [Agent Skills](https://agentskills.io)
  loader over any `io/fs.FS`; descriptions sit in the prompt, full instructions
  load only when a skill is used.
- **Deterministic ReAct loop** — dependency-aware parallel tool scheduling with
  `<output_of:ID>` refs, anti-loop detection, token-budget-aware pruning.
- **Streaming & HITL** — SSE streaming, human approvals, plan mode, and
  self-critique that keeps the best-scoring pass instead of the last one.
- **Honest terminals** — a `context_trace` event says exactly which messages
  pruning rewrote and why; a `degraded` terminal reports "the artifact landed,
  the bookkeeping did not" instead of forcing a turn into success or failure.
- **Custom tools** — one interface, schema derived from a Go struct; a middleware
  chain for logging, timing, rate limiting, and tracing.
- **Multi-provider** — OpenAI, Anthropic, Gemini, Vertex, and OpenAI-compatible
  backends, each in its own subpackage; multi-model routing; sampling controls.
- **Sub-agents & async** — sub-agent streaming, conversation forking, background
  workers, first-class task tracking.
- **Cross-session memory** — a post-session consolidator distills transcripts
  into notes the loader prepends to future sessions.
- **Observability** — OpenTelemetry traces + metrics (one trace per turn, nested
  LLM/tool spans, GenAI-convention token metrics), zero-cost when off.
- **Evaluation** — grade trajectory + answer + HITL with `pkg/eval` and a
  CI-ready `gopherevals` CLI.

## Documentation

| Guide | Use it for |
|---|---|
| [Getting started](docs/getting-started.md) | Install, the YAML builder, persistent sessions |
| [Tools](docs/tools.md) | Built-in tools, writing custom tools, middleware |
| [Skills](docs/skills.md) | Progressive disclosure — catalog in the prompt, instructions on demand |
| [Permissions & HITL](docs/permissions.md) | Confirmation gates, permission DSL, autonomous approvals |
| [Providers](docs/providers.md) | Providers, multi-model routing, sampling, multimodal |
| [Observability](docs/observability.md) | OpenTelemetry traces & metrics, collectors, debugging a conversation |
| [Evaluation](docs/evaluation.md) | Grade an agent with `pkg/eval` + `gopherevals` |

Full API reference: [pkg.go.dev](https://pkg.go.dev/github.com/hung12ct/gopheragent).

## Examples

| Example | What it shows |
|---|---|
| [`examples/demo`](./examples/demo) | Full chat UI — web research, memory sidebar, Python execution, live HITL, SSE streaming, OTLP export |
| [`examples/agent_eval`](./examples/agent_eval) | Agent evaluation — trajectory + answer + judge graders, JUnit/Markdown reports, CI gate |
| [`examples/creative_studio`](./examples/creative_studio) | AI Creative Director — DALL-E 3 images + Veo 2 video clips generated inline |
| [`examples/media_chat`](./examples/media_chat) | Media Q&A — upload image/video/doc, native multimodal history, multi-turn references |
| [`examples/hitl_server`](./examples/hitl_server) | Human-in-the-loop approvals over HTTP (async bridge) |
| [`examples/yaml_agents`](./examples/yaml_agents) | Multiple YAML-defined agents sharing a catalog, plus a skills-driven assistant |

```bash
cd examples/demo
printf "LLM_PROVIDER=openai\nOPENAI_API_KEY=sk-...\n" > .env
go run .
# open http://localhost:8888
```

## License

Apache 2.0
