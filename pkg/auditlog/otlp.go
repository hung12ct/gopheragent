package auditlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// encodeRecord turns a validated event into an OTLP LogRecord: the JSON event
// as the body, identity as attributes, and the ctx span as trace correlation.
func encodeRecord(ctx context.Context, e Event, now time.Time) (*logspb.LogRecord, error) {
	at := e.common().Time
	if at.IsZero() {
		at = now
	}
	body, err := eventJSON(e, at)
	if err != nil {
		return nil, err
	}
	id, err := newEventID()
	if err != nil {
		return nil, err
	}
	name := EventName(e)
	rec := &logspb.LogRecord{
		TimeUnixNano:         uint64(at.UnixNano()),
		ObservedTimeUnixNano: uint64(now.UnixNano()),
		SeverityNumber:       logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
		SeverityText:         "INFO",
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: string(body)}},
		Attributes: []*commonpb.KeyValue{
			strKV(attrEventName, name),
			strKV(attrEventID, id),
			strKV(attrEventType, name),
			strKV(attrTenantID, e.common().TenantID),
		},
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		tid, sid := sc.TraceID(), sc.SpanID()
		rec.TraceId, rec.SpanId = tid[:], sid[:]
		rec.Flags = uint32(sc.TraceFlags())
	}
	return rec, nil
}

// eventJSON marshals e with its kind, schema version, and resolved time.
func eventJSON(e Event, at time.Time) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("auditlog: encoding %s: %w", e.Kind(), err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("auditlog: encoding %s: %w", e.Kind(), err)
	}
	m["time"] = at.UTC().Format(time.RFC3339Nano)
	m["kind"] = e.Kind()
	m["schema_version"] = SchemaVersion
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("auditlog: encoding %s: %w", e.Kind(), err)
	}
	return out, nil
}

func newEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auditlog: event id: %w", err)
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
