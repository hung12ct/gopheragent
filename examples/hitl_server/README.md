# HITL Server Example

Minimal reference for **Human-In-The-Loop** tool approvals over HTTP.

The AgentLoop's `ConfirmHITL` callback is synchronous — it blocks the tool call
until a boolean decision arrives. This example shows how to bridge that
synchronous signature to an asynchronous HTTP approval endpoint so a web UI
can intervene in real time.

## Run

```bash
OPENAI_API_KEY=sk-... go run ./examples/hitl_server
```

Server listens on `:8080`:
- `POST /chat` — start a chat turn, stream SSE events back
- `POST /approve` — deliver an approval decision
- `GET /healthz`

## Demo

**Terminal 1** — start the chat:

```bash
curl -N -X POST http://localhost:8080/chat \
  -H "Content-Type: application/json" \
  -d '{"session_id":"demo","message":"Delete /tmp/foo using the shell tool."}'
```

When the agent wants to call the `shell` tool you will see:

```
event: approval_required
data: {"id":"appr-1713379200123456789","tool":"shell","args":"{\"command\":\"rm -rf /tmp/foo\"}"}
```

**Terminal 2** — approve (or deny):

```bash
curl -X POST http://localhost:8080/approve \
  -H "Content-Type: application/json" \
  -d '{"id":"appr-1713379200123456789","approved":true}'
```

The agent resumes and streams the rest of the response.

## How it works

`approvalBroker.Await` is called from inside `ConfirmHITL`. It:

1. Generates a stable ID and stores a `pendingApproval` (with a buffered
   decision channel) keyed by that ID.
2. Fires the `approval_required` SSE event so the client learns the ID.
3. Blocks on the decision channel until `/approve` arrives (or the request
   context is cancelled, in which case the call is auto-denied).

`/approve` does a map lookup and sends the boolean down the channel —
releasing the agent goroutine.

## Production notes

This is a **single-process reference**. For real deployments:

- **Persist pending approvals** (DB row with a TTL, Redis hash, etc.) so a
  restart doesn't strand the agent.
- **Route decisions across replicas** — if `/chat` lands on replica A and
  `/approve` lands on replica B, use pub/sub (Redis / NATS) to deliver the
  verdict. Keep the broker stateful only at the node that owns the pending
  call.
- **Add authn/authz on `/approve`** — otherwise anyone who guesses an ID can
  approve tool calls.
- **Add a timeout** — deny automatically after N minutes rather than blocking
  indefinitely.
- **Pass `sessionKey` into `ConfirmHITL`** — this example broadcasts the
  `approval_required` event to every open SSE channel because
  `ConfirmFunc`'s current signature does not carry `sessionKey`. A
  multi-tenant server must either extend the signature or route pending
  approvals through a session-scoped channel before entering the loop.
