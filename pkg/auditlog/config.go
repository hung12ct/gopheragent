package auditlog

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// OTLP transport protocols.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http/protobuf"
)

// Config holds the connection and identity settings for a Client.
type Config struct {
	// Endpoint is the collector address as host:port. Required.
	Endpoint string
	// Protocol is ProtocolGRPC (default) or ProtocolHTTP. HTTP posts to
	// /v1/logs on Endpoint.
	Protocol string

	// Insecure sends in plaintext. Local collectors only.
	Insecure bool
	// TLS is a complete TLS configuration. Mutually exclusive with the
	// file fields below. Nil (and no files) means system roots.
	TLS *tls.Config
	// CAFile verifies the collector; CertFile + KeyFile present a client
	// certificate (mTLS). PEM files.
	CAFile, CertFile, KeyFile string
	// Headers are sent with every export (gRPC metadata or HTTP headers),
	// e.g. an authorization token.
	Headers map[string]string
	// Gzip compresses export payloads.
	Gzip bool
	// Timeout bounds one export attempt; default 10s.
	Timeout time.Duration
	// MaxAttempts bounds retries of a transient export failure; default 3.
	MaxAttempts int

	// DefaultTenantID attributes records made by Instrument when the ctx
	// carries no Actor tenant — what a single-tenant app sets instead of
	// calling WithActor per request.
	DefaultTenantID string

	// HMACKey (at least 16 bytes) enables keyed argument fingerprints
	// (ToolInvocation.ArgsHMAC) and a signature on every record, which Verify
	// checks. Keep it out of the audit store: whoever holds it can forge
	// records. Nil: no fingerprints, records numbered but unsigned.
	HMACKey []byte

	// Mode is ModeBlocking (default) or ModeBatch. See Mode.
	Mode Mode
	// Batch tunes ModeBatch; ignored otherwise.
	Batch BatchConfig
	// OnError receives export failures in ModeBatch, where no caller is
	// waiting for them. Default: log.
	OnError func(ctx context.Context, err error)

	// Meter, when set, records the client's own health:
	// gopheragent.audit.records{kind, result=sent|failed|invalid|dropped} and
	// gopheragent.audit.export.duration. Alert on failed and dropped — a
	// broken audit pipeline is otherwise silent.
	Meter metric.Meter

	// ServiceName (required), ServiceNamespace, ServiceInstanceID, and
	// Environment identify the sender on the OTLP Resource. Restricted to
	// [A-Za-z0-9._/~-] — identifiers, not free text.
	ServiceName       string
	ServiceNamespace  string
	ServiceInstanceID string
	Environment       string
}

var resourceValueRE = regexp.MustCompile(`^[A-Za-z0-9._/~-]{1,128}$`)

// headerNameRE is the RFC 7230 token charset, lowercase-safe for gRPC metadata.
var headerNameRE = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,128}$`)

// validate checks cfg and applies defaults; it returns every violation.
func (cfg *Config) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf("auditlog: config: "+format, a...)) }

	if cfg.Endpoint == "" || strings.Contains(cfg.Endpoint, "://") {
		bad("Endpoint must be host:port, got %q", cfg.Endpoint)
	}
	switch cfg.Protocol {
	case "":
		cfg.Protocol = ProtocolGRPC
	case ProtocolGRPC, ProtocolHTTP:
	default:
		bad("Protocol %q unsupported (want %s or %s)", cfg.Protocol, ProtocolGRPC, ProtocolHTTP)
	}
	hasFiles := cfg.CAFile != "" || cfg.CertFile != "" || cfg.KeyFile != ""
	if cfg.TLS != nil && hasFiles {
		bad("TLS and CAFile/CertFile/KeyFile are mutually exclusive")
	}
	if cfg.Insecure && (cfg.TLS != nil || hasFiles) {
		bad("Insecure cannot be combined with TLS settings")
	}
	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		bad("CertFile and KeyFile must be set together")
	}
	for k, v := range cfg.Headers {
		if !headerNameRE.MatchString(k) {
			bad("header name %q is not a valid token", k)
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			bad("header %q value contains a control character", k)
		}
	}
	for name, v := range map[string]string{
		"ServiceName": cfg.ServiceName, "ServiceNamespace": cfg.ServiceNamespace,
		"ServiceInstanceID": cfg.ServiceInstanceID, "Environment": cfg.Environment,
	} {
		if v == "" && name != "ServiceName" {
			continue
		}
		if !resourceValueRE.MatchString(v) {
			bad("%s %q must match %s", name, v, resourceValueRE)
		}
	}
	if len(cfg.HMACKey) > 0 && len(cfg.HMACKey) < minKeyLen {
		bad("HMACKey must be at least %d bytes", minKeyLen)
	}
	if cfg.Mode != ModeBlocking && cfg.Mode != ModeBatch {
		bad("Mode %d unknown", cfg.Mode)
	}
	cfg.Batch.applyDefaults()
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	return errors.Join(errs...)
}

// tlsConfig resolves the client TLS settings; nil means plaintext.
func (cfg *Config) tlsConfig() (*tls.Config, error) {
	if cfg.Insecure {
		return nil, nil
	}
	if cfg.TLS != nil {
		return cfg.TLS.Clone(), nil
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("auditlog: reading CAFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("auditlog: CAFile %q holds no PEM certificate", cfg.CAFile)
		}
		tc.RootCAs = pool
	}
	if cfg.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("auditlog: loading client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}
