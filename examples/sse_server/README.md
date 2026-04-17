# SSE Chat Server

A minimal 5-minute quickstart that exposes a GopherAgent agent over a
Server-Sent Events HTTP endpoint. About 80 lines of code; a solid starting
point to copy into your own service.

## Run

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/sse_server
# 2026/04/17 12:00:00 gopheragent SSE server listening on :8080 (POST /chat)
```

## Stream from curl

```bash
curl -N -X POST http://localhost:8080/chat \
  -H "Content-Type: application/json" \
  -d '{"session_id":"demo","message":"Hello, who are you?"}'
```

You'll see SSE frames stream in real time:

```
event: thought
data: {"type":"thought","content":"The user greets me..."}

event: content
data: {"type":"content","content":"Hi! I'm a concise assistant..."}

event: done
data: {"type":"done","content":""}
```

## Consume from the browser

```html
<script>
  const evtSrc = new EventSource("/chat"); // GET-based clients need a wrapper
  evtSrc.addEventListener("content", (e) => {
    const { content } = JSON.parse(e.data);
    document.body.append(content);
  });
  evtSrc.addEventListener("done", () => evtSrc.close());
</script>
```

For POST-based streaming in the browser use `fetch()` + a
`ReadableStream.getReader()` — `EventSource` itself is GET-only.

## What this example demonstrates

- `RunIterationStream(ctx, sessionKey, userInput, chan)` — the streaming entry point.
- Framing each `StreamEvent` as an SSE event named after its `Type`
  (`content`, `thought`, `tool_call`, `action_required`, `error`, `done`).
- `X-Accel-Buffering: no` to prevent nginx from buffering chunks.
- `http.Flusher` for real-time delivery.
- Graceful shutdown: when the client disconnects, the request context is
  cancelled and the AgentLoop drains and returns.

## Next steps

- Add tools: register them with `tools.Registry` before constructing the loop.
- Add sessions persistence: swap `InMemSessionManager` for `FileSessionManager`
  or `MySQLSessionManager`.
- Add observability: `loop.OnEvent(telemetry.NewOTelHandler(tracer))`.
