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
