package auditlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// scopeName is the instrumentation scope on every audit record.
const scopeName = "github.com/hung12ct/gopheragent/pkg/auditlog"

// Log-record attribute keys.
const (
	attrEventName = "event.name"
	attrEventID   = "cloudevents.event_id"
	attrEventType = "cloudevents.event_type"
	attrTenantID  = "gopheragent.audit.tenant_id"
)

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intKV(k string, v uint64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(v)}}}
}

// buildResource maps the service identity onto the OTLP Resource.
func buildResource(cfg Config) *resourcepb.Resource {
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strKV("service.name", cfg.ServiceName)}}
	for k, v := range map[string]string{
		"service.namespace":           cfg.ServiceNamespace,
		"service.instance.id":         cfg.ServiceInstanceID,
		"deployment.environment.name": cfg.Environment,
	} {
		if v != "" {
			res.Attributes = append(res.Attributes, strKV(k, v))
		}
	}
	return res
}

// item is one queued event with the span context of the Send that produced
// it, so trace correlation survives batch mode's background encoding.
type item struct {
	ev Event
	sc trace.SpanContext
}

func itemsFor(ctx context.Context, events []Event) []item {
	sc := trace.SpanContextFromContext(ctx)
	items := make([]item, len(events))
	for i, e := range events {
		items[i] = item{ev: e, sc: sc}
	}
	return items
}

// encoder numbers and signs records. Each client is one stream: a random
// stream ID plus a monotonic sequence, so Verify can find deleted records.
type encoder struct {
	stream string
	seq    atomic.Uint64
	key    []byte // nil: records are numbered but unsigned
}

func newEncoder(key []byte) (*encoder, error) {
	id, err := newEventID()
	if err != nil {
		return nil, err
	}
	return &encoder{stream: id, key: key}, nil
}

// record turns a validated event into an OTLP LogRecord: the JSON event as
// the body, identity and integrity fields as attributes, and the event's span
// as trace correlation. The event ID and sequence are fixed here, before any
// retry, so a retried delivery is recognizably the same record.
func (enc *encoder) record(it item, now time.Time) (*logspb.LogRecord, error) {
	at := it.ev.common().Time
	if at.IsZero() {
		at = now
	}
	body, err := eventJSON(it.ev, at)
	if err != nil {
		return nil, err
	}
	id, err := newEventID()
	if err != nil {
		return nil, err
	}
	r := Record{Stream: enc.stream, Sequence: enc.seq.Add(1), EventID: id, EventName: EventName(it.ev), Body: string(body)}
	attrs := make([]*commonpb.KeyValue, 0, 7)
	attrs = append(attrs,
		strKV(attrEventName, r.EventName),
		strKV(attrEventID, r.EventID),
		strKV(attrEventType, r.EventName),
		strKV(attrTenantID, it.ev.common().TenantID),
		strKV(attrStream, r.Stream),
		intKV(attrSequence, r.Sequence),
	)
	if enc.key != nil {
		attrs = append(attrs, strKV(attrSignature, sign(enc.key, r)))
	}
	rec := &logspb.LogRecord{
		TimeUnixNano:         uint64(at.UnixNano()),
		ObservedTimeUnixNano: uint64(now.UnixNano()),
		SeverityNumber:       logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
		SeverityText:         "INFO",
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: r.Body}},
		Attributes:           attrs,
	}
	if it.sc.IsValid() {
		tid, sid := it.sc.TraceID(), it.sc.SpanID()
		rec.TraceId, rec.SpanId = tid[:], sid[:]
		rec.Flags = uint32(it.sc.TraceFlags())
	}
	return rec, nil
}

// eventJSON marshals e once and prefixes the envelope fields onto the raw
// object — no intermediate map, and a stable field order.
func eventJSON(e Event, at time.Time) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("auditlog: encoding %s: %w", e.Kind(), err)
	}
	buf := make([]byte, 0, len(raw)+96)
	buf = append(buf, `{"kind":"`...)
	buf = append(buf, e.Kind()...)
	buf = append(buf, `","schema_version":"`+SchemaVersion+`","time":"`...)
	buf = at.UTC().AppendFormat(buf, time.RFC3339Nano)
	buf = append(buf, '"')
	if len(raw) > 2 { // raw is a JSON object: splice its fields after ours
		buf = append(buf, ',')
		buf = append(buf, raw[1:]...)
	} else {
		buf = append(buf, '}')
	}
	return buf, nil
}

func newEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auditlog: random id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// buildRequest wraps records in one ExportLogsServiceRequest.
func buildRequest(res *resourcepb.Resource, recs []*logspb.LogRecord) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: res,
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope:      &commonpb.InstrumentationScope{Name: scopeName, Version: SchemaVersion},
			LogRecords: recs,
		}},
	}}}
}

// checkPartial turns a collector's partial rejection into an error: a record
// the collector refused is a record that was not written.
func checkPartial(resp *collogspb.ExportLogsServiceResponse) error {
	ps := resp.GetPartialSuccess()
	if ps == nil || ps.GetRejectedLogRecords() == 0 {
		return nil
	}
	return fmt.Errorf("%w: collector rejected %d record(s): %s", ErrRejected, ps.GetRejectedLogRecords(), ps.GetErrorMessage())
}
