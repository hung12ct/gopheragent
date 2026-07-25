# GopherAgent documentation

Start with the guide that matches what you are trying to do:

| Guide | Use it for |
|---|---|
| [Getting started](getting-started.md) | Install, the YAML builder, and persistent sessions |
| [Tools](tools.md) | Built-in tools, writing custom tools, and middleware |
| [Permissions & HITL](permissions.md) | Confirmation gates, the permission DSL, autonomous approvals |
| [Providers](providers.md) | OpenAI / Anthropic / Gemini / Vertex / compatible, routing, sampling, multimodal |
| [Observability](observability.md) | OpenTelemetry traces & metrics, running a collector, debugging a reported conversation |
| [Evaluation](evaluation.md) | Grade an agent's trajectory + answer + HITL with `pkg/eval` and the `gopherevals` CLI |

The root [README](../README.md) is intentionally short and serves as the project
landing page. Full API reference lives on
[pkg.go.dev](https://pkg.go.dev/github.com/hung12ct/gopheragent).
