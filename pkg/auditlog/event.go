package auditlog

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// SchemaVersion is the version suffix of every event name. It changes only on
// a breaking change to an event's fields.
const SchemaVersion = "v1"

// ErrInvalidEvent is wrapped by every validation failure.
var ErrInvalidEvent = errors.New("auditlog: invalid event")

// Field limits. Generous for real identifiers, tight enough that an audit
// record can't become an unbounded payload channel.
const (
	maxIDLen        = 256
	maxTypeLen      = 64
	maxTextLen      = 4096
	maxContextPairs = 32
	maxListLen      = 64
)

// Common is embedded in every event: who acted, for which tenant, and when.
type Common struct {
	// Time of the action; zero means "now" at send time. Encoded once, as the
	// envelope's "time" field.
	Time time.Time `json:"-"`
	// TenantID owns the data the action touched. Required.
	TenantID string `json:"tenant_id"`
	// ActorID is the user (or service principal) that initiated the action.
	ActorID string `json:"actor_id,omitempty"`
	// ImpersonatorID is set when ActorID acted on behalf of someone else.
	ImpersonatorID string `json:"impersonator_id,omitempty"`
	// SourceIPs are the client addresses the request came from.
	SourceIPs []string `json:"source_ips,omitempty"`
	// AppID identifies the calling application.
	AppID string `json:"app_id,omitempty"`
	// SessionID is the agent session (conversation) the action belongs to.
	SessionID string `json:"session_id,omitempty"`
	// Context is free-form, bounded key/value metadata.
	Context map[string]string `json:"context,omitempty"`
}

// Event is implemented by the event types in this package only.
type Event interface {
	// Kind is the event's type name, e.g. "ToolInvocation".
	Kind() string
	// Validate reports every constraint violation, joined; each wraps
	// ErrInvalidEvent.
	Validate() error
	common() Common
}

// EventName is the OTLP event name for e: "gopheragent.audit.<Kind>.v1".
func EventName(e Event) string { return "gopheragent.audit." + e.Kind() + "." + SchemaVersion }

// Outcome is the result of an audited action.
type Outcome string

const (
	OutcomeStarted Outcome = "started" // fail-closed pre-execution record
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
)

// ToolInvocation records one tool execution by an agent.
type ToolInvocation struct {
	Common
	Tool     string        `json:"tool"`
	CallID   string        `json:"call_id,omitempty"`
	Outcome  Outcome       `json:"outcome"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration_ns,omitempty"`
	// ArgsHMAC is a keyed fingerprint of the arguments ("hmac-sha256:<hex>",
	// see Hash): it matches a record to a known request without storing the
	// arguments, and without the key it can't be brute-forced back to them.
	// Empty when the client has no HMACKey.
	ArgsHMAC string `json:"args_hmac,omitempty"`
	// Args is the raw argument JSON (LevelRequest and above).
	Args string `json:"args,omitempty"`
	// Result is the tool's model-facing text (LevelRequestResponse).
	Result string `json:"result,omitempty"`
}

// Decision is a human or policy verdict.
type Decision string

const (
	DecisionApproved Decision = "approved"
	DecisionDenied   Decision = "denied"
	DecisionTimedOut Decision = "timed_out"
	DecisionCanceled Decision = "cancelled"
	DecisionAllow    Decision = "allow"
	DecisionDeny     Decision = "deny"
	DecisionConfirm  Decision = "confirm"
)

// ApprovalDecision records a human-in-the-loop verdict on a tool call.
type ApprovalDecision struct {
	Common
	Tool     string   `json:"tool"`
	CallID   string   `json:"call_id,omitempty"`
	Decision Decision `json:"decision"` // approved | denied | timed_out | cancelled
}

// PolicyDecision records an automated permission-policy verdict.
type PolicyDecision struct {
	Common
	Tool     string   `json:"tool"`
	Decision Decision `json:"decision"` // allow | deny | confirm
	Policy   string   `json:"policy,omitempty"`
}

// SecurityViolation records a blocked request: a rejected prompt, a
// malicious or unauthorized request.
type SecurityViolation struct {
	Common
	Category string `json:"category"` // e.g. "hook_rejected", "unauthorized"
	Detail   string `json:"detail,omitempty"`
}

// DataAccess records a read of tenant data.
type DataAccess struct {
	Common
	ObjectType string   `json:"object_type"`
	ObjectID   string   `json:"object_id"`
	Fields     []string `json:"fields,omitempty"`
	Channel    string   `json:"channel,omitempty"` // e.g. "tool:sql_agent", "api"
}

// DataModification records a create, update, or delete of tenant data.
type DataModification struct {
	Common
	ObjectType string   `json:"object_type"`
	ObjectID   string   `json:"object_id"`
	Operation  string   `json:"operation"` // create | update | delete
	Changes    []Change `json:"changes,omitempty"`
	Outcome    Outcome  `json:"outcome"`
}

// ConfigurationChange records a change to system or tenant configuration.
type ConfigurationChange struct {
	Common
	ObjectType string   `json:"object_type"`
	ObjectID   string   `json:"object_id"`
	Changes    []Change `json:"changes"`
	Outcome    Outcome  `json:"outcome"`
}

// Change is one attribute's before/after value.
type Change struct {
	Name     string `json:"name"`
	OldValue string `json:"old_value,omitempty"`
	NewValue string `json:"new_value,omitempty"`
}

// Custom carries an application-specific audit event. Name becomes part of
// the event identity; Payload must be JSON-serializable.
type Custom struct {
	Common
	Name    string         `json:"name"`
	Payload map[string]any `json:"payload,omitempty"`
}

func (ToolInvocation) Kind() string      { return "ToolInvocation" }
func (ApprovalDecision) Kind() string    { return "ApprovalDecision" }
func (PolicyDecision) Kind() string      { return "PolicyDecision" }
func (SecurityViolation) Kind() string   { return "SecurityViolation" }
func (DataAccess) Kind() string          { return "DataAccess" }
func (DataModification) Kind() string    { return "DataModification" }
func (ConfigurationChange) Kind() string { return "ConfigurationChange" }
func (Custom) Kind() string              { return "Custom" }

func (e ToolInvocation) common() Common      { return e.Common }
func (e ApprovalDecision) common() Common    { return e.Common }
func (e PolicyDecision) common() Common      { return e.Common }
func (e SecurityViolation) common() Common   { return e.Common }
func (e DataAccess) common() Common          { return e.Common }
func (e DataModification) common() Common    { return e.Common }
func (e ConfigurationChange) common() Common { return e.Common }
func (e Custom) common() Common              { return e.Common }

func (e ToolInvocation) Validate() error {
	v := newValidator(e)
	v.required("tool", e.Tool, maxTypeLen)
	v.max("call_id", e.CallID, maxIDLen)
	v.oneOf("outcome", string(e.Outcome), OutcomeStarted, OutcomeSuccess, OutcomeFailure)
	v.max("error", e.Error, maxTextLen)
	v.max("args", e.Args, maxTextLen)
	v.max("result", e.Result, maxTextLen)
	return v.err()
}

func (e ApprovalDecision) Validate() error {
	v := newValidator(e)
	v.required("tool", e.Tool, maxTypeLen)
	v.max("call_id", e.CallID, maxIDLen)
	v.oneOf("decision", string(e.Decision), DecisionApproved, DecisionDenied, DecisionTimedOut, DecisionCanceled)
	return v.err()
}

func (e PolicyDecision) Validate() error {
	v := newValidator(e)
	v.required("tool", e.Tool, maxTypeLen)
	v.oneOf("decision", string(e.Decision), DecisionAllow, DecisionDeny, DecisionConfirm)
	v.max("policy", e.Policy, maxTypeLen)
	return v.err()
}

func (e SecurityViolation) Validate() error {
	v := newValidator(e)
	v.required("category", e.Category, maxTypeLen)
	v.max("detail", e.Detail, maxTextLen)
	return v.err()
}

func (e DataAccess) Validate() error {
	v := newValidator(e)
	v.required("object_type", e.ObjectType, maxTypeLen)
	v.required("object_id", e.ObjectID, maxIDLen)
	v.list("fields", e.Fields, maxTypeLen)
	v.max("channel", e.Channel, maxTypeLen)
	return v.err()
}

func (e DataModification) Validate() error {
	v := newValidator(e)
	v.required("object_type", e.ObjectType, maxTypeLen)
	v.required("object_id", e.ObjectID, maxIDLen)
	v.oneOf("operation", e.Operation, "create", "update", "delete")
	v.changes(e.Changes, false)
	v.oneOf("outcome", string(e.Outcome), OutcomeSuccess, OutcomeFailure)
	return v.err()
}

func (e ConfigurationChange) Validate() error {
	v := newValidator(e)
	v.required("object_type", e.ObjectType, maxTypeLen)
	v.required("object_id", e.ObjectID, maxIDLen)
	v.changes(e.Changes, true)
	v.oneOf("outcome", string(e.Outcome), OutcomeSuccess, OutcomeFailure)
	return v.err()
}

var customNameRE = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

func (e Custom) Validate() error {
	v := newValidator(e)
	if !customNameRE.MatchString(e.Name) {
		v.fail("name", "must match "+customNameRE.String())
	}
	return v.err()
}

// validator accumulates violations for one event, starting with Common.
type validator struct {
	kind string
	errs []error
}

func newValidator(e Event) *validator {
	v := &validator{kind: e.Kind()}
	c := e.common()
	v.required("tenant_id", c.TenantID, maxIDLen)
	v.max("actor_id", c.ActorID, maxIDLen)
	v.max("impersonator_id", c.ImpersonatorID, maxIDLen)
	v.max("app_id", c.AppID, maxIDLen)
	v.max("session_id", c.SessionID, maxIDLen)
	v.list("source_ips", c.SourceIPs, maxTypeLen)
	if len(c.Context) > maxContextPairs {
		v.fail("context", fmt.Sprintf("has %d entries, max %d", len(c.Context), maxContextPairs))
	}
	for k, val := range c.Context {
		v.required("context key", k, maxTypeLen)
		v.max("context["+k+"]", val, maxTextLen)
	}
	return v
}

func (v *validator) fail(field, msg string) {
	v.errs = append(v.errs, fmt.Errorf("%w: %s.%s %s", ErrInvalidEvent, v.kind, field, msg))
}

func (v *validator) required(field, val string, maxLen int) {
	if val == "" {
		v.fail(field, "is required")
		return
	}
	v.max(field, val, maxLen)
}

func (v *validator) max(field, val string, maxLen int) {
	if len(val) > maxLen {
		v.fail(field, fmt.Sprintf("is %d bytes, max %d", len(val), maxLen))
	}
}

func (v *validator) oneOf(field, val string, allowed ...any) {
	for _, a := range allowed {
		if fmt.Sprint(a) == val {
			return
		}
	}
	v.fail(field, fmt.Sprintf("is %q, want one of %v", val, allowed))
}

func (v *validator) list(field string, vals []string, maxLen int) {
	if len(vals) > maxListLen {
		v.fail(field, fmt.Sprintf("has %d entries, max %d", len(vals), maxListLen))
	}
	for _, s := range vals {
		v.required(field+" entry", s, maxLen)
	}
}

func (v *validator) changes(cs []Change, required bool) {
	if required && len(cs) == 0 {
		v.fail("changes", "is required")
	}
	if len(cs) > maxListLen {
		v.fail("changes", fmt.Sprintf("has %d entries, max %d", len(cs), maxListLen))
	}
	for _, c := range cs {
		v.required("changes.name", c.Name, maxTypeLen)
		v.max("changes.old_value", c.OldValue, maxTextLen)
		v.max("changes.new_value", c.NewValue, maxTextLen)
	}
}

func (v *validator) err() error { return errors.Join(v.errs...) }
