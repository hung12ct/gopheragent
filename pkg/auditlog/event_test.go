package auditlog

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := Common{TenantID: "acme"}
	cases := []struct {
		name    string
		ev      Event
		wantErr string // substring; "" means valid
	}{
		{"valid tool invocation", ToolInvocation{Common: ok, Tool: "refund", Outcome: OutcomeSuccess}, ""},
		{"tenant required", ToolInvocation{Tool: "refund", Outcome: OutcomeSuccess}, "tenant_id is required"},
		{"bad outcome", ToolInvocation{Common: ok, Tool: "refund", Outcome: "maybe"}, "outcome"},
		{"tool too long", ToolInvocation{Common: ok, Tool: strings.Repeat("x", 65), Outcome: OutcomeSuccess}, "tool is 65 bytes"},
		{"approval decision enum", ApprovalDecision{Common: ok, Tool: "t", Decision: DecisionAllow}, "decision"},
		{"policy decision", PolicyDecision{Common: ok, Tool: "t", Decision: DecisionDeny}, ""},
		{"config change needs changes", ConfigurationChange{Common: ok, ObjectType: "flag", ObjectID: "f1", Outcome: OutcomeSuccess}, "changes is required"},
		{"data modification op", DataModification{Common: ok, ObjectType: "order", ObjectID: "1", Operation: "upsert", Outcome: OutcomeSuccess}, "operation"},
		{"data access", DataAccess{Common: ok, ObjectType: "order", ObjectID: "1", Fields: []string{"total"}}, ""},
		{"custom name pattern", Custom{Common: ok, Name: "Bad Name"}, "name must match"},
		{"custom ok", Custom{Common: ok, Name: "invoice.sent", Payload: map[string]any{"n": 1}}, ""},
		{"context bounded", SecurityViolation{Common: Common{TenantID: "a", Context: bigContext()}, Category: "x"}, "context has 33 entries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ev.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("validation error must wrap ErrInvalidEvent: %v", err)
			}
		})
	}
}

func TestValidate_ReportsAllViolations(t *testing.T) {
	err := ToolInvocation{Outcome: "x"}.Validate()
	for _, want := range []string{"tenant_id", "tool", "outcome"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q violation in %v", want, err)
		}
	}
}

func TestEventName(t *testing.T) {
	if got := EventName(DataAccess{}); got != "gopheragent.audit.DataAccess.v1" {
		t.Fatalf("EventName = %q", got)
	}
}

func bigContext() map[string]string {
	m := map[string]string{}
	for i := 0; i < 33; i++ {
		m[strings.Repeat("k", i+1)] = "v"
	}
	return m
}

func TestConfigValidate(t *testing.T) {
	base := func() Config { return Config{Endpoint: "c:4317", ServiceName: "svc"} }
	cases := []struct {
		name    string
		mut     func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"endpoint required", func(c *Config) { c.Endpoint = "" }, "Endpoint"},
		{"endpoint no scheme", func(c *Config) { c.Endpoint = "http://c:4318" }, "host:port"},
		{"service name required", func(c *Config) { c.ServiceName = "" }, "ServiceName"},
		{"resource charset", func(c *Config) { c.ServiceNamespace = "a b;rm" }, "ServiceNamespace"},
		{"header crlf", func(c *Config) { c.Headers = map[string]string{"authorization": "x\r\nevil: 1"} }, "control character"},
		{"header name", func(c *Config) { c.Headers = map[string]string{"bad name": "v"} }, "not a valid token"},
		{"protocol", func(c *Config) { c.Protocol = "http/json" }, "unsupported"},
		{"insecure plus tls", func(c *Config) { c.Insecure, c.CAFile = true, "ca.pem" }, "Insecure"},
		{"cert without key", func(c *Config) { c.CertFile = "c.pem" }, "together"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mut(&cfg)
			err := cfg.validate()
			if tc.wantErr == "" {
				if err != nil || cfg.Protocol != ProtocolGRPC || cfg.MaxAttempts != 3 || cfg.Timeout == 0 {
					t.Fatalf("valid config: err=%v cfg=%+v", err, cfg)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
