# Audit log — who did what, over OTLP

`pkg/auditlog` records an agent's consequential actions — tool executions,
human approvals, permission verdicts, rejected requests, data and
configuration changes — as structured audit events, shipped as **OTLP log
records**. The collector you already run for traces and metrics carries the
audit trail; route the `gopheragent.audit.*` events to an append-only store
with a collector pipeline.

Audit is not observability: telemetry may be sampled and dropped, audit must
not. So by default (`ModeBlocking`) **`Send` returns only after the collector
acknowledged the record, or with the error that prevented it.** Nothing is
buffered; a nil return means written. High-volume events can opt into
`ModeBatch` — see [Delivery modes](#delivery-modes).

## Set up

```go
c, err := auditlog.New(ctx, auditlog.Config{
    Endpoint:    "collector:4317",       // host:port
    ServiceName: "billing-agent",        // required, on the OTLP Resource
    CAFile:      "/etc/ca.pem",          // or TLS: &tls.Config{...}; Insecure for local
    CertFile:    "/etc/client.pem",      // + KeyFile → mTLS
    KeyFile:     "/etc/client-key.pem",
    Headers:     map[string]string{"authorization": "Bearer " + token},
    HMACKey:     key,                    // ≥16 bytes: keyed fingerprints + signed records
    Meter:       otel.Meter("audit"),    // self-health metrics
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
| `WithPolicy(fn)` / `WithLevel(l)` | Per-tool audit level, chosen once at registration (below) |
| `WithFailClosed()` | Write a `started` record **before** the tool runs — synchronously, even in `ModeBatch` — and refuse to run it if that write fails: no audit record, no action |
| `WithDefaultTenant(t)` | Tenant for single-tenant deployments with no `Actor` on ctx |
| `WithAllowDecisions()` | `AuditPermissions` also records allow verdicts |
| `WithErrorHandler(fn)` | Receives write failures the integration cannot return (default: log) |

**Levels** decide how much of a call is recorded — raise them only where the
record needs the payload, since arguments and results often hold PII:

| Level | Records |
|---|---|
| `LevelNone` | Nothing — the tool is left unwrapped |
| `LevelMetadata` (default) | Outcome, duration, error, keyed args fingerprint |
| `LevelRequest` | + raw arguments |
| `LevelRequestResponse` | + the tool's result text |

```go
auditlog.ToolMiddleware(c, auditlog.WithPolicy(func(d tools.ToolDescriptor) auditlog.Level {
    if d.RequiresConfirmation { return auditlog.LevelRequest }
    return auditlog.LevelMetadata
}))
```

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

## Delivery modes

| Mode | `Send` returns | Use for |
|---|---|---|
| `ModeBlocking` (default) | after the collector acknowledged | agent actions, approvals, policy verdicts |
| `ModeBatch` | after validation and queueing | high-volume events (data access) |

`ModeBatch` validates synchronously — an invalid event still fails at `Send`
— then a single background worker exports up to `Batch.MaxBatch` (400)
records per request, or whatever is queued after `Batch.MaxWait` (1s). The
queue holds `Batch.BufferSize` (10000) events; beyond that `Send` **drops and
returns `ErrBufferFull`**, and the drop is counted. Export failures go to
`Config.OnError`. A crash loses what is queued, so `Flush` at checkpoints and
always `Close` (which drains). `SendSync` bypasses the queue in either mode.

**Two destinations.** `Multi(a, b)` sends to both in parallel and succeeds when
at least one does, so one outage doesn't lose the trail; `MultiAll` requires
both. `NewJSONLWriter(w)` is a local destination writing one JSON line per
event:

```go
audit := auditlog.Multi(collectorClient, auditlog.NewJSONLWriter(file))
```

## Integrity: fingerprints, signatures, verification

With `HMACKey` set:

- **Keyed fingerprints.** `ToolInvocation.ArgsHMAC` is
  `hmac-sha256:<hex>` of the arguments. A plain hash of low-entropy arguments
  (`{"user_id":"42"}`) can be brute-forced back to them; a keyed one can't
  without the key. Find the records of a known request with
  `auditlog.Hash(key, knownArgs)`. Without a key, no fingerprint is recorded.
- **Signed, sequenced records.** Every record carries
  `gopheragent.audit.stream` (random per client), a monotonic
  `gopheragent.audit.sequence`, and `gopheragent.audit.signature` — an HMAC
  over stream, sequence, event ID, event name, and body.

`auditlog.Verify(key, records)` (build `records` with `RecordFromOTLP`) reports:

| Finding | Means |
|---|---|
| `BadSignature` | modified, forged, renumbered, or signed with another key |
| `Gaps` | deleted records — or a `Send` that returned an error (correlate with `failed` metrics) |
| `Conflicts` | a sequence number reused by a different event |

A retried export that delivered twice is recognized (same event ID) and is not
a finding. Truncating the *end* of a stream isn't detectable from the records
alone — compare against the latest sequence your monitoring saw. Keep the key
out of the audit store: whoever holds it can produce valid signatures.

## Self-health metrics

With `Config.Meter` set: `gopheragent.audit.records{kind, result}` with
`result` = `sent` | `failed` | `invalid` | `dropped`, and
`gopheragent.audit.export.duration{error}`. Alert on `failed` and `dropped` —
a broken audit pipeline is otherwise silent.

## Wire format

One OTLP `LogRecord` per event, severity `INFO`:

- **Body** — the event as JSON, plus `kind`, `schema_version`, and the resolved `time`.
- **Attributes** — `event.name` = `gopheragent.audit.<Kind>.v1`,
  `cloudevents.event_id` (random 128-bit, fixed before retries),
  `cloudevents.event_type`, `gopheragent.audit.tenant_id`, plus the integrity
  fields above.
- **Resource** — `service.name`, `service.namespace`, `service.instance.id`,
  `deployment.environment.name`: *who sent it*. The tenant on each event says
  *whose data it is*.
- **Trace context** — `trace_id`/`span_id` from the ctx span, when present.

The `v1` suffix changes only on a breaking change to an event's fields.

## Design choices

- **Synchronous by default, batching opt-in.** A buffered record is lost on a
  crash, which is the failure audit exists to prevent — so buffering is a
  choice you make per client, with drops counted and reported, never a
  default. `Send(ctx, events...)` batches explicitly in either mode.
- **Sequence numbers + per-record HMAC, not a hash chain.** A chain would
  serialize every send behind a lock held across network I/O, and a failed
  send would read as tampering. Per-record signatures detect modification and
  forgery; sequence gaps detect deletion; sends stay concurrent. HMAC fits one
  trust domain; if verifiers must not be able to sign, an asymmetric scheme is
  the upgrade.
- **One encode pass.** The event is marshaled once and the envelope fields
  prefixed onto the raw JSON — no intermediate map (~10× faster than
  re-marshaling, stable field order for signing).
- **Thin OTLP client, not the OTel logs SDK.** Built directly on the OTLP
  protobuf types and gRPC already in the module: export errors come back to
  the caller instead of a global error handler, and no pre-1.0 SDK is pulled
  in.
- **Go structs, not generated protobuf schemas.** Validation lives in
  `Validate()` methods — no codegen toolchain, same boundary guarantee.
