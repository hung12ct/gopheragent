package auditlog

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"

	"github.com/hung12ct/gopheragent/internal/latewire"
	"github.com/hung12ct/gopheragent/pkg/agent"
)

// Environment variables read by FromEnv. Only the endpoint is required; with
// it unset, auditing is off.
const (
	EnvEndpoint    = "GOPHERAGENT_AUDIT_ENDPOINT"     // host:port — set it to turn auditing on
	EnvRequired    = "GOPHERAGENT_AUDIT_REQUIRED"     // true: a missing endpoint is an error, not "off"
	EnvProtocol    = "GOPHERAGENT_AUDIT_PROTOCOL"     // grpc (default) | http/protobuf
	EnvInsecure    = "GOPHERAGENT_AUDIT_INSECURE"     // true: plaintext, local collectors only
	EnvServiceName = "GOPHERAGENT_AUDIT_SERVICE_NAME" // default: OTEL_SERVICE_NAME, then "gopheragent"
	EnvTenantID    = "GOPHERAGENT_AUDIT_TENANT_ID"    // default tenant when ctx has no Actor; default "default"
	EnvHMACKey     = "GOPHERAGENT_AUDIT_HMAC_KEY"     // ≥16 bytes: keyed fingerprints + signed records
	EnvHeaders     = "GOPHERAGENT_AUDIT_HEADERS"      // k=v,k2=v2 — e.g. authorization=Bearer%20token
	EnvMode        = "GOPHERAGENT_AUDIT_MODE"         // blocking (default) | batch
	EnvCAFile      = "GOPHERAGENT_AUDIT_CA_FILE"
	EnvCertFile    = "GOPHERAGENT_AUDIT_CERT_FILE"
	EnvKeyFile     = "GOPHERAGENT_AUDIT_KEY_FILE"
)

// FromEnv builds a Client from GOPHERAGENT_AUDIT_* environment variables.
// With GOPHERAGENT_AUDIT_ENDPOINT unset it returns a nil *Client and no
// error — a disabled client whose methods do nothing — so the same code runs
// with auditing on or off. Where auditing is mandatory, set
// GOPHERAGENT_AUDIT_REQUIRED=true so a missing or misspelled endpoint fails
// startup instead of silently disabling the trail.
//
// The client's self-health metrics go to the global OTel MeterProvider, so
// with telemetry enabled (otelsetup.Setup, before or after) failed and dropped
// records are visible without extra wiring:
//
//	audit, err := auditlog.FromEnv(ctx)
//	if err != nil { ... }          // set but invalid configuration
//	defer audit.Close(ctx)
//	audit.Instrument(loop)
func FromEnv(ctx context.Context) (*Client, error) {
	cfg, ok, err := configFromEnv(os.Getenv)
	if err != nil || !ok {
		return nil, err
	}
	cfg.Meter = otel.GetMeterProvider().Meter(scopeName)
	return New(ctx, cfg)
}

func configFromEnv(getenv func(string) string) (Config, bool, error) {
	cfg := Config{Endpoint: strings.TrimSpace(getenv(EnvEndpoint))}
	if cfg.Endpoint == "" {
		if v := getenv(EnvRequired); v != "" {
			required, err := strconv.ParseBool(v)
			if err != nil {
				return cfg, false, fmt.Errorf("auditlog: %s=%q: %w", EnvRequired, v, err)
			}
			if required {
				return cfg, false, fmt.Errorf("auditlog: %s is set but %s is empty", EnvRequired, EnvEndpoint)
			}
		}
		return cfg, false, nil
	}
	cfg.Protocol = strings.TrimSpace(getenv(EnvProtocol))
	cfg.ServiceName = firstSet(getenv(EnvServiceName), getenv("OTEL_SERVICE_NAME"), "gopheragent")
	cfg.DefaultTenantID = firstSet(getenv(EnvTenantID), "default")
	cfg.CAFile, cfg.CertFile, cfg.KeyFile = getenv(EnvCAFile), getenv(EnvCertFile), getenv(EnvKeyFile)
	if k := getenv(EnvHMACKey); k != "" {
		cfg.HMACKey = []byte(k)
	}
	if v := getenv(EnvInsecure); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, false, fmt.Errorf("auditlog: %s=%q: %w", EnvInsecure, v, err)
		}
		cfg.Insecure = b
	}
	switch m := strings.ToLower(strings.TrimSpace(getenv(EnvMode))); m {
	case "", "blocking":
	case "batch":
		cfg.Mode = ModeBatch
	default:
		return cfg, false, fmt.Errorf("auditlog: %s=%q: want blocking or batch", EnvMode, m)
	}
	headers, err := parseHeaders(getenv(EnvHeaders))
	if err != nil {
		return cfg, false, err
	}
	cfg.Headers = headers
	return cfg, true, nil
}

// parseHeaders reads the OTEL_EXPORTER_OTLP_HEADERS format: comma-separated
// key=value pairs, values percent-decoded.
func parseHeaders(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("auditlog: %s: %q is not key=value", EnvHeaders, pair)
		}
		dv, err := url.PathUnescape(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("auditlog: %s: header %q: %w", EnvHeaders, k, err)
		}
		out[k] = dv
	}
	return out, nil
}

func firstSet(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// Instrument audits an already-built loop in one call: every tool in
// loop.Tools (ToolMiddleware), the HITL callback (AuditConfirm), the
// permission policy (AuditPermissions), and hook rejections (EventHandler).
// opts are passed to each, e.g. WithFailClosed or WithPolicy. Records fall
// back to Config.DefaultTenantID when the ctx carries no Actor.
//
// Call it last — after ConfirmHITL, Permissions, and the tools are set; ones
// set afterwards are not audited (a warning names them on the loop's first
// event). Tools are never double-wrapped, but the
// callback, policy, and event handler are, so call it once per loop. A nil
// client changes nothing. Returns loop for chaining.
func (c *Client) Instrument(loop *agent.AgentLoop, opts ...Option) *agent.AgentLoop {
	if c == nil || loop == nil {
		return loop
	}
	if c.cfg.DefaultTenantID != "" {
		opts = append([]Option{WithDefaultTenant(c.cfg.DefaultTenantID)}, opts...) // caller's options win
	}
	latewire.Watch(loop, "auditlog", true)
	if loop.Tools != nil {
		loop.Tools.Use(ToolMiddleware(c, opts...))
	}
	if loop.ConfirmHITL != nil {
		loop.ConfirmHITL = AuditConfirm(loop.ConfirmHITL, c, opts...)
	}
	if loop.Permissions != nil {
		loop.Permissions = AuditPermissions(loop.Permissions, c, opts...)
	}
	return loop.OnEvent(EventHandler(c, opts...))
}
