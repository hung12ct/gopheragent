# Audit log — who did what, over OTLP

`pkg/auditlog` records an agent's consequential actions — tool executions,
human approvals, permission verdicts, rejected requests, data and
configuration changes — as structured audit events, shipped as **OTLP log
records**. The collector you already run for traces and metrics carries the
audit trail; route the `gopheragent.audit.*` events to an append-only store
with a collector pipeline.

Audit is not observability: telemetry may be sampled and dropped, audit must
not. So the client is built around one rule — **`Send` returns only after the
collector acknowledged the record, or with the error that prevented it.**
Nothing is buffered; a nil return means written.

## Set up

```go
c, err := auditlog.New(ctx, auditlog.Config{
    Endpoint:    "collector:4317",       // host:port
    ServiceName: "billing-agent",        // required, on the OTLP Resource
    CAFile:      "/etc/ca.pem",          // or TLS: &tls.Config{...}; Insecure for local
    CertFile:    "/etc/client.pem",      // + KeyFile → mTLS
    KeyFile:     "/etc/client-key.pem",
    Headers:     map[string]string{"authorization": "Bearer " + token},
})
if err != nil { /* invalid config */ }
defer c.Close(ctx)
```

`Protocol: auditlog.ProtocolHTTP` posts to `/v1/logs` instead of gRPC;
`Gzip` compresses. Transient failures (gRPC `Unavailable`/`ResourceExhausted`,
HTTP 429/502/503/504, network errors) are retried with backoff up to
`MaxAttempts` (default 3), each attempt bounded by `Timeout` (default 10s).
A collector's **partial rejection is an error** (`ErrRejected`).

Config is validated up front: service identity values are restricted to
`[A-Za-z0-9._/~-]`, header names must be tokens, and header values may not
contain CR/LF.

## Audit an agent with no per-tool code

```go
// At the request boundary: who is acting, for which tenant.
ctx = auditlog.WithActor(ctx, auditlog.Actor{TenantID: tenant, UserID: user, SourceIPs: []string{ip}})

reg.Register(tools.Chain(refundTool, auditlog.ToolMiddleware(c)))           // ToolInvocation
loop.ConfirmHITL = auditlog.AuditConfirm(askHuman, c)                       // ApprovalDecision
loop.Permissions = auditlog.AuditPermissions(rules, c)                      // PolicyDecision
loop.OnEvent(auditlog.EventHandler(c))                                      // SecurityViolation (hook rejections)
```

With the YAML builder, `catalog.Use(auditlog.ToolMiddleware(c))` audits every
built tool. Records pick up the session key from the loop and, when the ctx
carries a span, its trace and span IDs — an auditor can jump from a record to
the trace of the run that produced it.

| Option | Effect |
|---|---|
| `WithFailClosed()` | Write a `started` record **before** the tool runs and refuse to run it if that write fails — no audit record, no action |
| `WithFilter(fn)` | Audit only matching tools, e.g. `d.RequiresConfirmation`; others stay unwrapped |
| `WithArgs()` | Record raw arguments (off by default — a SHA-256 fingerprint is always recorded) |
| `WithDefaultTenant(t)` | Tenant for single-tenant deployments with no `Actor` on ctx |
| `WithAllowDecisions()` | `AuditPermissions` also records allow verdicts |
| `WithErrorHandler(fn)` | Receives write failures the integration cannot return (default: log) |

Records of an action's outcome are written on a ctx detached from
cancellation, so a timed-out approval or a cancelled run is still recorded.
The wrappers never change a verdict or a tool result.

## Events

Every event embeds `Common` — `tenant_id` (required), `actor_id`,
`impersonator_id`, `source_ips`, `app_id`, `session_id`, `context`, `time` —
and is validated before sending (required fields, enums, length limits). An
invalid event fails at your boundary with `ErrInvalidEvent`; if any event in a
`Send` is invalid, none is sent.

| Event | Records |
|---|---|
| `ToolInvocation` | A tool run: outcome (`started`/`success`/`failure`), error, duration, args fingerprint |
| `ApprovalDecision` | A human verdict: `approved`, `denied`, `timed_out`, `cancelled` |
| `PolicyDecision` | A permission verdict: `allow`, `deny`, `confirm` |
| `SecurityViolation` | A blocked request (e.g. `hook_rejected`) |
| `DataAccess` | A read of tenant data (object type/id, fields, channel) |
| `DataModification` | A create/update/delete with before/after changes |
| `ConfigurationChange` | A configuration change with before/after values |
| `Custom` | An application event: `Name` (`[a-z][a-z0-9_.]*`) + JSON payload |

Emit the data and custom events yourself where the action happens:

```go
err := c.Send(ctx, auditlog.DataModification{
    Common:     auditlog.Common{TenantID: tenant, ActorID: user},
    ObjectType: "order", ObjectID: id, Operation: "update", Outcome: auditlog.OutcomeSuccess,
    Changes:    []auditlog.Change{{Name: "status", OldValue: "paid", NewValue: "refunded"}},
})
```

## Wire format

One OTLP `LogRecord` per event, severity `INFO`:

- **Body** — the event as JSON, plus `kind`, `schema_version`, and the resolved `time`.
- **Attributes** — `event.name` = `gopheragent.audit.<Kind>.v1`,
  `cloudevents.event_id` (random 128-bit), `cloudevents.event_type`,
  `gopheragent.audit.tenant_id`.
- **Resource** — `service.name`, `service.namespace`, `service.instance.id`,
  `deployment.environment.name`: *who sent it*. The tenant on each event says
  *whose data it is*.
- **Trace context** — `trace_id`/`span_id` from the ctx span, when present.

The `v1` suffix changes only on a breaking change to an event's fields.

## Design choices

- **Synchronous by design.** No background batching: a buffered record is lost
  on a crash, which is the failure audit exists to prevent. Batch explicitly
  with `Send(ctx, events...)` — one request, all-or-nothing validation.
- **Thin OTLP client, not the OTel logs SDK.** Built directly on the OTLP
  protobuf types and gRPC already in the module: export errors come back to
  the caller instead of a global error handler, and no pre-1.0 SDK is pulled
  in.
- **Go structs, not generated protobuf schemas.** Validation lives in
  `Validate()` methods — no codegen toolchain, same boundary guarantee.
