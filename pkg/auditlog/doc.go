// Package auditlog records who did what — tool executions, human approvals,
// policy decisions, data and configuration changes — as structured audit
// events shipped over OTLP logs, so the collector infrastructure already
// running for traces and metrics carries the audit trail too.
//
// Three layers:
//
//   - Events (event.go): a small, versioned schema. Every event embeds
//     Common (tenant, actor, session, source IPs, time) and is validated
//     before it leaves the process, so a malformed record fails at your
//     boundary rather than disappearing at the collector.
//   - Client (client.go, otlp.go): encodes events as OTLP LogRecords and
//     exports them over gRPC or HTTP/protobuf, with TLS/mTLS. Send is
//     synchronous and returns the export error — including a collector's
//     partial rejection — because an audit record the caller believes was
//     written but was not is the failure mode audit exists to prevent.
//   - Agent integration (agent.go): a tool middleware, a ConfirmFunc wrapper,
//     a PermissionChecker decorator, and an EventHandler that audit an agent's
//     actions without per-tool code. WithFailClosed makes the tool middleware
//     refuse to execute when its audit record cannot be written.
//
// Service identity (name, namespace, instance) rides the OTLP Resource; the
// tenant the data belongs to rides every event. When the caller's ctx carries
// a span, the record carries its trace and span IDs, linking the audit trail to
// the trace of the run that produced it.
//
// Minimal use:
//
//	c, err := auditlog.New(ctx, auditlog.Config{Endpoint: "collector:4317", ServiceName: "billing-agent"})
//	if err != nil { ... }
//	defer c.Close(ctx)
//
//	ctx = auditlog.WithActor(ctx, auditlog.Actor{TenantID: "acme", UserID: "u-42"})
//	reg.Register(tools.Chain(refundTool, auditlog.ToolMiddleware(c)))
package auditlog
