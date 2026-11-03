package auditlog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"
	"unicode/utf8"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// Actor identifies who is acting, carried on ctx from the request boundary so
// every audit record of the turn is attributed without threading arguments.
type Actor struct {
	TenantID       string
	UserID         string
	ImpersonatorID string
	AppID          string
	SourceIPs      []string
}

type actorKey struct{}

// WithActor returns a ctx carrying a for the audit integrations.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFromContext returns the Actor set by WithActor.
func ActorFromContext(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// Option configures the agent integrations.
type Option func(*options)

type options struct {
	defaultTenant string
	captureArgs   bool
	filter        func(tools.ToolDescriptor) bool
	failClosed    bool
	recordAllow   bool
	onError       func(ctx context.Context, err error)
}

// WithDefaultTenant attributes records to tenant when ctx carries no Actor
// tenant — for single-tenant deployments.
func WithDefaultTenant(tenant string) Option { return func(o *options) { o.defaultTenant = tenant } }

// WithArgs records raw tool arguments. Off by default: arguments often hold
// PII, and the SHA-256 fingerprint is recorded either way.
func WithArgs() Option { return func(o *options) { o.captureArgs = true } }

// WithFilter audits only tools for which keep returns true; others are left
// unwrapped (no overhead). E.g. audit only side-effecting tools:
//
//	auditlog.WithFilter(func(d tools.ToolDescriptor) bool { return d.RequiresConfirmation })
func WithFilter(keep func(tools.ToolDescriptor) bool) Option {
	return func(o *options) { o.filter = keep }
}

// WithFailClosed makes ToolMiddleware write a "started" record before the
// tool runs and refuse to run it if that write fails: no audit record, no
// action. The outcome record follows execution as usual.
func WithFailClosed() Option { return func(o *options) { o.failClosed = true } }

// WithAllowDecisions makes AuditPermissions record allow verdicts too, not
// only deny and confirm.
func WithAllowDecisions() Option { return func(o *options) { o.recordAllow = true } }

// WithErrorHandler receives audit write failures the integration cannot
// return to a caller. The default logs them.
func WithErrorHandler(fn func(ctx context.Context, err error)) Option {
	return func(o *options) { o.onError = fn }
}

func newOptions(opts []Option) *options {
	o := &options{onError: func(_ context.Context, err error) { log.Printf("[gopheragent] %v", err) }}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// common builds the shared fields from the ctx Actor and agent session.
func (o *options) common(ctx context.Context) Common {
	c := Common{TenantID: o.defaultTenant}
	if a, ok := ActorFromContext(ctx); ok {
		if a.TenantID != "" {
			c.TenantID = a.TenantID
		}
		c.ActorID, c.ImpersonatorID, c.AppID, c.SourceIPs = a.UserID, a.ImpersonatorID, a.AppID, a.SourceIPs
	}
	c.SessionID, _ = agent.SessionKeyFromContext(ctx)
	return c
}

// send records e on a ctx detached from cancellation: the record of a
// timed-out or cancelled action must still be written. The client's
// per-attempt timeout still bounds it.
func send(ctx context.Context, e Emitter, ev Event) error {
	return e.Send(context.WithoutCancel(ctx), ev)
}

// ToolMiddleware audits every execution of the wrapped tool as a
// ToolInvocation (outcome, duration, error, argument fingerprint). A failed
// outcome write goes to the error handler; with WithFailClosed, a failed
// pre-execution write stops the tool from running.
func ToolMiddleware(e Emitter, opts ...Option) tools.Middleware {
	o := newOptions(opts)
	return func(next tools.Tool) tools.Tool {
		desc := next.Descriptor()
		if o.filter != nil && !o.filter(desc) {
			return next
		}
		return &auditedTool{Tool: next, name: desc.Name, e: e, o: o}
	}
}

type auditedTool struct {
	tools.Tool
	name string
	e    Emitter
	o    *options
}

func (t *auditedTool) Execute(ctx context.Context, argsJSON string) (tools.Result, error) {
	sum := sha256.Sum256([]byte(argsJSON))
	inv := ToolInvocation{
		Common:     t.o.common(ctx),
		Tool:       t.name,
		CallID:     tools.ToolCallIDFromContext(ctx),
		ArgsSHA256: hex.EncodeToString(sum[:]),
	}
	if t.o.captureArgs {
		inv.Args = truncate(argsJSON, maxTextLen)
	}
	if t.o.failClosed {
		started := inv
		started.Outcome = OutcomeStarted
		if err := t.e.Send(ctx, started); err != nil {
			return tools.Result{}, fmt.Errorf("auditlog: refusing to run %s: audit record not written: %w", t.name, err)
		}
	}

	start := time.Now()
	res, err := t.Tool.Execute(ctx, argsJSON)
	inv.Duration = time.Since(start)
	inv.Outcome = OutcomeSuccess
	if err != nil {
		inv.Outcome, inv.Error = OutcomeFailure, truncate(err.Error(), maxTextLen)
	}
	if sendErr := send(ctx, t.e, inv); sendErr != nil {
		t.o.onError(ctx, fmt.Errorf("auditlog: %s outcome record: %w", t.name, sendErr))
	}
	return res, err
}

// AuditConfirm wraps a HITL ConfirmFunc so every human verdict — approved,
// denied, timed out, or cancelled — is recorded as an ApprovalDecision.
func AuditConfirm(inner agent.ConfirmFunc, e Emitter, opts ...Option) agent.ConfirmFunc {
	o := newOptions(opts)
	return func(ctx context.Context, toolName, argsJSON string) bool {
		approved := inner(ctx, toolName, argsJSON)
		d := DecisionDenied
		switch {
		case approved:
			d = DecisionApproved
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			d = DecisionTimedOut
		case errors.Is(ctx.Err(), context.Canceled):
			d = DecisionCanceled
		}
		ev := ApprovalDecision{Common: o.common(ctx), Tool: toolName, CallID: tools.ToolCallIDFromContext(ctx), Decision: d}
		if err := send(ctx, e, ev); err != nil {
			o.onError(ctx, fmt.Errorf("auditlog: %s approval record: %w", toolName, err))
		}
		return approved
	}
}

// AuditPermissions decorates a PermissionChecker so deny and confirm verdicts
// (and allow, with WithAllowDecisions) are recorded as PolicyDecisions. The
// verdict itself is never changed.
func AuditPermissions(inner agent.PermissionChecker, e Emitter, opts ...Option) agent.PermissionChecker {
	return &auditedPermissions{inner: inner, e: e, o: newOptions(opts)}
}

type auditedPermissions struct {
	inner agent.PermissionChecker
	e     Emitter
	o     *options
}

func (p *auditedPermissions) Check(ctx context.Context, toolName, argsJSON string) agent.PermissionDecision {
	verdict := p.inner.Check(ctx, toolName, argsJSON)
	var d Decision
	switch verdict {
	case agent.PermissionDeny:
		d = DecisionDeny
	case agent.PermissionConfirm:
		d = DecisionConfirm
	case agent.PermissionAllow:
		if !p.o.recordAllow {
			return verdict
		}
		d = DecisionAllow
	default:
		return verdict // no opinion: nothing decided, nothing to record
	}
	ev := PolicyDecision{Common: p.o.common(ctx), Tool: toolName, Decision: d}
	if err := send(ctx, p.e, ev); err != nil {
		p.o.onError(ctx, fmt.Errorf("auditlog: %s policy record: %w", toolName, err))
	}
	return verdict
}

// EventHandler records security-relevant loop events: a request rejected by a
// policy hook becomes a SecurityViolation. Register it with
// loop.OnEvent(auditlog.EventHandler(client)).
func EventHandler(e Emitter, opts ...Option) agent.EventHandler {
	o := newOptions(opts)
	return func(ctx context.Context, sessionKey string, ev agent.StreamEvent) {
		p, ok := ev.Payload.(agent.ErrorEvent)
		if !ok {
			return
		}
		var hr *agent.HookRejectedError
		if !errors.As(p.Err, &hr) {
			return
		}
		c := o.common(ctx)
		if c.SessionID == "" {
			c.SessionID = sessionKey
		}
		detail := ""
		if hr.Cause != nil {
			detail = truncate(hr.Cause.Error(), maxTextLen)
		}
		if err := send(ctx, e, SecurityViolation{Common: c, Category: "hook_rejected", Detail: detail}); err != nil {
			o.onError(ctx, fmt.Errorf("auditlog: security record: %w", err))
		}
	}
}

// truncate cuts s to at most n bytes on a UTF-8 boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
