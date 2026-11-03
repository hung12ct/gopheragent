package auditlog

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

var (
	// ErrRejected wraps a collector's refusal of one or more records.
	ErrRejected = errors.New("auditlog: records rejected")
	// ErrClosed is returned by Send after Close.
	ErrClosed = errors.New("auditlog: client closed")
)

// Emitter is the narrow interface the agent integrations depend on, so tests
// and adopters can substitute their own sink.
type Emitter interface {
	Send(ctx context.Context, events ...Event) error
}

// exporter is one transport. retryable reports whether a failure is
// transient (worth another attempt).
type exporter interface {
	export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (resp *collogspb.ExportLogsServiceResponse, retryable bool, err error)
	close() error
}

// Client ships audit events to an OTLP collector. It is safe for concurrent
// use.
type Client struct {
	cfg    Config
	res    *resourcepb.Resource
	exp    exporter
	closed atomic.Bool
	now    func() time.Time
}

var _ Emitter = (*Client)(nil)

// New validates cfg and connects. The gRPC connection is lazy: a collector
// that is down surfaces on the first Send, not here.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	tc, err := cfg.tlsConfig()
	if err != nil {
		return nil, err
	}
	var exp exporter
	if cfg.Protocol == ProtocolHTTP {
		exp = newHTTPExporter(cfg, tc)
	} else if exp, err = newGRPCExporter(ctx, cfg, tc); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, res: buildResource(cfg), exp: exp, now: time.Now}, nil
}

// Send validates every event, then exports them in one request. It returns
// only after the collector acknowledged the records, or with the error that
// prevented it — nothing is buffered, so a nil return means written and a
// crash afterwards cannot lose the record. If any event is invalid, none is
// sent.
func (c *Client) Send(ctx context.Context, events ...Event) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if len(events) == 0 {
		return nil
	}
	recs, err := c.encode(ctx, events)
	if err != nil {
		return err
	}
	return c.exportWithRetry(ctx, buildRequest(c.res, recs))
}

func (c *Client) encode(ctx context.Context, events []Event) ([]*logspb.LogRecord, error) {
	var verrs []error
	for _, e := range events {
		if e == nil {
			verrs = append(verrs, fmt.Errorf("%w: nil event", ErrInvalidEvent))
			continue
		}
		if err := e.Validate(); err != nil {
			verrs = append(verrs, err)
		}
	}
	if err := errors.Join(verrs...); err != nil {
		return nil, err
	}
	now := c.now()
	recs := make([]*logspb.LogRecord, 0, len(events))
	for _, e := range events {
		rec, err := encodeRecord(ctx, e, now)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// exportWithRetry retries transient failures with exponential backoff
// (100ms, 200ms, ...) up to MaxAttempts, always bounded by ctx.
func (c *Client) exportWithRetry(ctx context.Context, req *collogspb.ExportLogsServiceRequest) error {
	backoff := 100 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
		resp, retryable, err := c.exp.export(attemptCtx, req)
		cancel()
		if err == nil {
			return checkPartial(resp)
		}
		lastErr = err
		if !retryable || attempt == c.cfg.MaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("auditlog: export: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return fmt.Errorf("auditlog: export: %w", lastErr)
}

// Close releases the connection. Send fails with ErrClosed afterwards.
func (c *Client) Close(context.Context) error {
	if c.closed.Swap(true) {
		return nil
	}
	if err := c.exp.close(); err != nil {
		return fmt.Errorf("auditlog: close: %w", err)
	}
	return nil
}
