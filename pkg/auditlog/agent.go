package auditlog

import (
	"context"
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

// Level is how much of a tool call ToolMiddleware records.
type Level int

const (
	// LevelNone: don't audit the tool (it is left unwrapped).
	LevelNone Level = iota
	// LevelMetadata (default): outcome, duration, error, keyed args fingerprint.
	LevelMetadata
	// LevelRequest: Metadata plus the raw arguments.
	LevelRequest
	// LevelRequestResponse: Request plus the tool's model-facing result text.
	LevelRequestResponse
)

type options struct {
	defaultTenant string
	policy        func(tools.ToolDescriptor) Level
	failClosed    bool
	recordAllow   bool
	onError       func(ctx context.Context, err error)
}

// WithDefaultTenant attributes records to tenant when ctx carries no Actor
// tenant — for single-tenant deployments.
func WithDefaultTenant(tenant string) Option { return func(o *options) { o.defaultTenant = tenant } }

// WithPolicy picks each tool's audit Level once, when the tool is wrapped —
// the first matching rule of a policy, evaluated at registration, not per
// call. Arguments and results often hold PII, so raise the level only where
// the record needs them:
//
//	auditlog.WithPolicy(func(d tools.ToolDescriptor) auditlog.Level {
//	    if d.RequiresConfirmation { return auditlog.LevelRequest }
//	    return auditlog.LevelMetadata
//	})
func WithPolicy(level func(tools.ToolDescriptor) Level) Option {
	return func(o *options) { o.policy = level }
}

// WithLevel applies one Level to every tool.
func WithLevel(l Level) Option {
	return WithPolicy(func(tools.ToolDescriptor) Level { return l })
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
// ToolInvocation at the tool's Level (see WithPolicy; default
// LevelMetadata). The argument fingerprint is keyed: it is recorded only when
// e holds an HMAC key (Config.HMACKey). A failed outcome write goes to the
// error handler; with WithFailClosed, a failed pre-execution write stops the
// tool from running.
func ToolMiddleware(e Emitter, opts ...Option) tools.Middleware {
	o := newOptions(opts)
	fp, _ := e.(Fingerprinter)
	return func(next tools.Tool) tools.Tool {
		if _, done := next.(*auditedTool); done {
			return next // already audited: wrapping again would double every record
		}
		desc := next.Descriptor()
		level := LevelMetadata
		if o.policy != nil {
			level = o.policy(desc)
		}
		if level <= LevelNone {
			return next
		}
		return &auditedTool{Tool: next, name: desc.Name, level: level, e: e, fp: fp, o: o}
	}
}

type auditedTool struct {
	tools.Tool
	name  string
	level Level
	e     Emitter
	fp    Fingerprinter // nil: no fingerprints
	o     *options
}

func (t *auditedTool) Execute(ctx context.Context, argsJSON string) (tools.Result, error) {
	inv := ToolInvocation{Common: t.o.common(ctx), Tool: t.name, CallID: tools.ToolCallIDFromContext(ctx)}
	if t.fp != nil {
		inv.ArgsHMAC = t.fp.Fingerprint(argsJSON)
	}
	if t.level >= LevelRequest {
		inv.Args = truncate(argsJSON, maxTextLen)
	}
	if t.o.failClosed {
		if err := t.writeStarted(ctx, inv); err != nil {
			return tools.Result{}, err
		}
	}

	start := time.Now()
	res, err := t.Tool.Execute(ctx, argsJSON)
	inv.Duration = time.Since(start)
	inv.Outcome = OutcomeSuccess
	if err != nil {
		inv.Outcome, inv.Error = OutcomeFailure, truncate(err.Error(), maxTextLen)
	}
	if t.level >= LevelRequestResponse {
		inv.Result = truncate(res.Text, maxTextLen)
	}
	if sendErr := send(ctx, t.e, inv); sendErr != nil {
		t.o.onError(ctx, fmt.Errorf("auditlog: %s outcome record: %w", t.name, sendErr))
	}
	return res, err
}

// writeStarted writes the fail-closed pre-execution record synchronously —
// bypassing any batch buffer, since a queued record is not a written one.
func (t *auditedTool) writeStarted(ctx context.Context, inv ToolInvocation) error {
	inv.Outcome = OutcomeStarted
	var err error
	if s, ok := t.e.(SyncEmitter); ok {
		err = s.SendSync(ctx, inv)
	} else {
		err = t.e.Send(ctx, inv)
	}
	if err != nil {
		return fmt.Errorf("auditlog: refusing to run %s: audit record not written: %w", t.name, err)
	}
	return nil
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
