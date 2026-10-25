# Permissions & human-in-the-loop

When a tool returns `RequiresConfirmation() = true`, the loop denies the call
**unless one of**:

- A `loop.ConfirmHITL` hook returns `true` (human approves), or
- A `loop.Permissions` rule returns `PermissionAllow` (policy auto-approves).

With neither, the call is denied and the model receives a directive
"permission gate" message telling it to ask the user for approval.

## Pre-approve trusted tools (autonomous agents)

For agents with no human reviewer, pre-approve trusted tools with the
permission DSL (deny-over-allow ordering):

```go
loop.Permissions = agent.NewPermissionRuleSet().
    Allow("call_sql_agent").     // bypass HITL for this tool
    Deny("dangerous_tool")       // deny always wins
```

`Allow` / `Deny` accept glob patterns, so you can gate families of tools.

## Opt a tool out of confirmation

Or remove the confirmation requirement at the source:

```go
sqlTool := builtin.NewSQLAgentTool(db, "", sm, provider).
    WithRequiresConfirmation(false)
```

See [`examples/hitl_server`](../examples/hitl_server) for approvals delivered
over HTTP with an async bridge.

## Durable approvals (answers that arrive hours later)

`ConfirmHITL` blocks the run until it returns, so a restart loses every
open approval. Where users answer much later (chat apps, email), set
`DeferApprovals` instead:

```go
loop.DeferApprovals = true

for ev := range loop.RunText(ctx, sessionKey, text) {
    if p, ok := ev.Payload.(agent.ActionRequiredEvent); ok {
        askUser(sessionKey, p.ID, p.Tool, p.Args) // e.g. a chat message
    }
    // the run ends with errors.Is(err, agent.ErrApprovalPending)
}

// Later, from any process that shares the SessionManager:
seq, err := loop.Resume(ctx, sessionKey, map[string]bool{approvalID: true})
```

The gated call is saved in session history without a result, so every
`SessionManager` backend keeps it across restarts. Other calls in the same
wave still run; calls in later waves wait, since they may depend on it.
`Resume` runs approved calls, records denials, and continues the turn. Calls
left without a decision stay pending. `PendingApprovals(ctx, sessionKey)`
lists what is open, for asking again after a restart or for routing a reply
to `Resume` instead of `Run`.

- A new `Run` on the session seals pending calls as cancelled, which is
  how a user moving on reads to the model.
- `Resume` does not lock the session. Route each session's answers
  through one goroutine, or an approved call can run twice.
- Sub-agent workers and the plan-mode gate still block on their callbacks.
