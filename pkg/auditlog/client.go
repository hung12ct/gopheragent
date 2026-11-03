package auditlog

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

var (
	// ErrRejected wraps a collector's refusal of one or more records.
	ErrRejected = errors.New("auditlog: records rejected")
	// ErrClosed is returned by Send after Close.
	ErrClosed = errors.New("auditlog: client closed")
	// ErrBufferFull is returned by Send in ModeBatch when the buffer has no
	// room: those events were dropped and counted.
	ErrBufferFull = errors.New("auditlog: batch buffer full")
)

// Mode selects when Send returns.
type Mode int

const (
	// ModeBlocking (default): Send returns after the collector acknowledged
	// the records. Nothing is buffered; a nil return means written.
	ModeBlocking Mode = iota
	// ModeBatch: Send validates synchronously, then queues; a background
	// worker exports in batches. Higher throughput and no per-event network
	// latency, but a crash loses queued records and a full buffer drops
	// (ErrBufferFull). Call Flush or Close to drain.
	ModeBatch
)

// Emitter is the narrow interface the agent integrations depend on, so tests
// and adopters can substitute their own sink.
type Emitter interface {
	Send(ctx context.Context, events ...Event) error
}

// SyncEmitter is implemented by emitters that can bypass their buffering.
// WithFailClosed uses it: a "started" record must be written, not queued,
// before the tool may run.
type SyncEmitter interface {
	SendSync(ctx context.Context, events ...Event) error
}

// Fingerprinter is implemented by emitters configured with an HMAC key.
type Fingerprinter interface {
	Fingerprint(value string) string
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
	cfg     Config
	res     *resourcepb.Resource
	exp     exporter
	enc     *encoder
	metrics *clientMetrics
	batch   *batcher // nil in ModeBlocking
	now     func() time.Time

	mu     sync.RWMutex // guards closed against in-flight batch enqueues
	closed bool
}

var (
	_ Emitter       = (*Client)(nil)
	_ SyncEmitter   = (*Client)(nil)
	_ Fingerprinter = (*Client)(nil)
)

// New validates cfg and connects. The gRPC connection is lazy: a collector
// that is down surfaces on the first Send, not here.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.OnError == nil {
		cfg.OnError = func(_ context.Context, err error) { log.Printf("[gopheragent] %v", err) }
	}
	tc, err := cfg.tlsConfig()
	if err != nil {
		return nil, err
	}
	enc, err := newEncoder(cfg.HMACKey)
	if err != nil {
		return nil, err
	}
	var exp exporter
	if cfg.Protocol == ProtocolHTTP {
		exp = newHTTPExporter(cfg, tc)
	} else if exp, err = newGRPCExporter(ctx, cfg, tc); err != nil {
		return nil, err
	}
	c := &Client{cfg: cfg, res: buildResource(cfg), exp: exp, enc: enc, metrics: newClientMetrics(cfg.Meter), now: time.Now}
	if cfg.Mode == ModeBatch {
		c.batch = newBatcher(c, cfg.Batch)
	}
	return c, nil
}

// Send validates every event and delivers them per the client's Mode. If any
// event is invalid, none is sent.
func (c *Client) Send(ctx context.Context, events ...Event) error {
	c.mu.RLock()
	defer c.mu.RUnlock() // held only across the non-blocking enqueue, never I/O
	if c.closed {
		return ErrClosed
	}
	if err := c.validate(ctx, events); err != nil || len(events) == 0 {
		return err
	}
	if c.batch != nil {
		return c.batch.enqueue(ctx, itemsFor(ctx, events))
	}
	return c.sendItems(ctx, itemsFor(ctx, events))
}

// SendSync delivers synchronously regardless of Mode, like ModeBlocking.
func (c *Client) SendSync(ctx context.Context, events ...Event) error {
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if err := c.validate(ctx, events); err != nil || len(events) == 0 {
		return err
	}
	return c.sendItems(ctx, itemsFor(ctx, events))
}

// Fingerprint returns Hash(HMACKey, value), or "" without a key.
func (c *Client) Fingerprint(value string) string {
	if c.cfg.HMACKey == nil {
		return ""
	}
	return Hash(c.cfg.HMACKey, value)
}

// Flush blocks until every event queued before the call is exported (or its
// failure reported to OnError). A no-op in ModeBlocking.
func (c *Client) Flush(ctx context.Context) error {
	if c.batch == nil {
		return nil
	}
	return c.batch.flush(ctx)
}

// Close drains the batch buffer (ModeBatch) and releases the connection.
// Send fails with ErrClosed afterwards.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	var drainErr error
	if c.batch != nil {
		drainErr = c.batch.close(ctx)
	}
	if err := c.exp.close(); err != nil {
		return errors.Join(drainErr, fmt.Errorf("auditlog: close: %w", err))
	}
	return drainErr
}

func (c *Client) validate(ctx context.Context, events []Event) error {
	var verrs []error
	for _, e := range events {
		if e == nil {
			verrs = append(verrs, fmt.Errorf("%w: nil event", ErrInvalidEvent))
		} else if err := e.Validate(); err != nil {
			verrs = append(verrs, err)
		}
	}
	err := errors.Join(verrs...)
	if err != nil {
		c.metrics.count(ctx, events, resultInvalid)
	}
	return err
}

// sendItems encodes and exports one request, recording the outcome.
func (c *Client) sendItems(ctx context.Context, items []item) error {
	now := c.now()
	recs := make([]*logspb.LogRecord, 0, len(items))
	for _, it := range items {
		rec, err := c.enc.record(it, now)
		if err != nil {
			return err
		}
		recs = append(recs, rec)
	}
	start := time.Now()
	err := c.exportWithRetry(ctx, buildRequest(c.res, recs))
	c.metrics.observe(ctx, time.Since(start), err)
	result := resultSent
	if err != nil {
		result = resultFailed
	}
	c.metrics.countItems(ctx, items, result)
	return err
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

// Self-metric results.
const (
	resultSent    = "sent"
	resultFailed  = "failed"
	resultInvalid = "invalid"
	resultDropped = "dropped"
)

var allKinds = []string{
	ToolInvocation{}.Kind(), ApprovalDecision{}.Kind(), PolicyDecision{}.Kind(), SecurityViolation{}.Kind(),
	DataAccess{}.Kind(), DataModification{}.Kind(), ConfigurationChange{}.Kind(), Custom{}.Kind(),
}

// clientMetrics holds the self-health instruments. Attribute sets for every
// (kind, result) pair are built once, so counting allocates nothing.
type clientMetrics struct {
	records  metric.Int64Counter
	duration metric.Float64Histogram
	sets     map[[2]string]metric.MeasurementOption
	okSet    metric.MeasurementOption
	errSet   metric.MeasurementOption
}

func newClientMetrics(m metric.Meter) *clientMetrics {
	if m == nil {
		return nil
	}
	cm := &clientMetrics{sets: make(map[[2]string]metric.MeasurementOption, len(allKinds)*4)}
	cm.records, _ = m.Int64Counter("gopheragent.audit.records",
		metric.WithUnit("{record}"), metric.WithDescription("Audit records by kind and result."))
	cm.duration, _ = m.Float64Histogram("gopheragent.audit.export.duration",
		metric.WithUnit("s"), metric.WithDescription("Audit export request latency."))
	for _, k := range allKinds {
		for _, r := range []string{resultSent, resultFailed, resultInvalid, resultDropped} {
			cm.sets[[2]string{k, r}] = metric.WithAttributeSet(attribute.NewSet(
				attribute.String("kind", k), attribute.String("result", r)))
		}
	}
	cm.okSet = metric.WithAttributeSet(attribute.NewSet(attribute.Bool("error", false)))
	cm.errSet = metric.WithAttributeSet(attribute.NewSet(attribute.Bool("error", true)))
	return cm
}

func (cm *clientMetrics) count(ctx context.Context, events []Event, result string) {
	if cm == nil || cm.records == nil {
		return
	}
	for _, e := range events {
		if e != nil {
			cm.records.Add(ctx, 1, cm.sets[[2]string{e.Kind(), result}])
		}
	}
}

func (cm *clientMetrics) countItems(ctx context.Context, items []item, result string) {
	if cm == nil || cm.records == nil {
		return
	}
	for _, it := range items {
		cm.records.Add(ctx, 1, cm.sets[[2]string{it.ev.Kind(), result}])
	}
}

func (cm *clientMetrics) observe(ctx context.Context, d time.Duration, err error) {
	if cm == nil || cm.duration == nil {
		return
	}
	set := cm.okSet
	if err != nil {
		set = cm.errSet
	}
	cm.duration.Record(ctx, d.Seconds(), set)
}
