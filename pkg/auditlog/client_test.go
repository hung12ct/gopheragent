package auditlog

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/trace"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// logsReceiver is an in-process OTLP logs collector. failFirst makes the
// first N exports fail with failCode; partial makes it reject records.
type logsReceiver struct {
	collogspb.UnimplementedLogsServiceServer
	mu        sync.Mutex
	reqs      []*collogspb.ExportLogsServiceRequest
	md        []metadata.MD
	calls     atomic.Int32
	failFirst int32
	failCode  codes.Code
	partial   int64
}

func (r *logsReceiver) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	n := r.calls.Add(1)
	if n <= r.failFirst {
		return nil, status.Error(r.failCode, "injected")
	}
	md, _ := metadata.FromIncomingContext(ctx)
	r.mu.Lock()
	r.reqs, r.md = append(r.reqs, req), append(r.md, md)
	r.mu.Unlock()
	resp := &collogspb.ExportLogsServiceResponse{}
	if r.partial > 0 {
		resp.PartialSuccess = &collogspb.ExportLogsPartialSuccess{RejectedLogRecords: r.partial, ErrorMessage: "tenant unknown"}
	}
	return resp, nil
}

func (r *logsReceiver) records() []*logspb.LogRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*logspb.LogRecord
	for _, req := range r.reqs {
		for _, rl := range req.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				out = append(out, sl.GetLogRecords()...)
			}
		}
	}
	return out
}

func startGRPCReceiver(t *testing.T, r *logsReceiver) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	collogspb.RegisterLogsServiceServer(srv, r)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// httpHandler serves OTLP/HTTP logs, decoding gzip when sent.
func httpHandler(r *logsReceiver, headers *http.Header) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body io.Reader = req.Body
		if req.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(req.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			body = zr
		}
		raw, _ := io.ReadAll(body)
		var in collogspb.ExportLogsServiceRequest
		if req.URL.Path != "/v1/logs" || proto.Unmarshal(raw, &in) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		*headers = req.Header.Clone()
		resp, _ := r.Export(context.Background(), &in)
		out, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}
}

func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	if cfg.ServiceName == "" {
		cfg.ServiceName = "svc"
	}
	c, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func attr(rec *logspb.LogRecord, key string) string {
	for _, kv := range rec.GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

var sampleEvent = DataModification{
	Common:     Common{TenantID: "acme", ActorID: "u-42", SessionID: "s1"},
	ObjectType: "order", ObjectID: "o-7", Operation: "update", Outcome: OutcomeSuccess,
	Changes: []Change{{Name: "status", OldValue: "open", NewValue: "refunded"}},
}

func TestClient_GRPCRecordShape(t *testing.T) {
	r := &logsReceiver{}
	c := newTestClient(t, Config{Endpoint: startGRPCReceiver(t, r), Insecure: true,
		ServiceNamespace: "payments", Headers: map[string]string{"authorization": "Bearer t"}})

	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	if err := c.Send(ctx, sampleEvent); err != nil {
		t.Fatalf("Send: %v", err)
	}

	recs := r.records()
	if len(recs) != 1 {
		t.Fatalf("got %d records", len(recs))
	}
	rec := recs[0]
	if attr(rec, attrEventName) != "gopheragent.audit.DataModification.v1" || attr(rec, attrTenantID) != "acme" || len(attr(rec, attrEventID)) != 32 {
		t.Fatalf("attributes = %v", rec.GetAttributes())
	}
	if !bytes.Equal(rec.GetTraceId(), tid[:]) || !bytes.Equal(rec.GetSpanId(), sid[:]) {
		t.Fatal("record not correlated with the ctx span")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(rec.GetBody().GetStringValue()), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["kind"] != "DataModification" || body["tenant_id"] != "acme" || body["object_id"] != "o-7" || body["time"] == "" {
		t.Fatalf("body = %v", body)
	}
	res := r.reqs[0].GetResourceLogs()[0].GetResource().GetAttributes()
	if len(res) != 2 || r.md[0].Get("authorization")[0] != "Bearer t" {
		t.Fatalf("resource=%v metadata=%v", res, r.md[0])
	}
}

func TestClient_InvalidEventSendsNothing(t *testing.T) {
	r := &logsReceiver{}
	c := newTestClient(t, Config{Endpoint: startGRPCReceiver(t, r), Insecure: true})
	err := c.Send(context.Background(), sampleEvent, ToolInvocation{Tool: "x", Outcome: OutcomeSuccess})
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("err = %v, want ErrInvalidEvent", err)
	}
	if r.calls.Load() != 0 {
		t.Fatal("a batch with an invalid event must send nothing")
	}
}

func TestClient_PartialRejectionIsAnError(t *testing.T) {
	r := &logsReceiver{partial: 1}
	c := newTestClient(t, Config{Endpoint: startGRPCReceiver(t, r), Insecure: true})
	if err := c.Send(context.Background(), sampleEvent); !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "tenant unknown") {
		t.Fatalf("err = %v, want ErrRejected with collector message", err)
	}
}

func TestClient_RetriesTransientOnly(t *testing.T) {
	r := &logsReceiver{failFirst: 2, failCode: codes.Unavailable}
	c := newTestClient(t, Config{Endpoint: startGRPCReceiver(t, r), Insecure: true})
	if err := c.Send(context.Background(), sampleEvent); err != nil || r.calls.Load() != 3 {
		t.Fatalf("err=%v calls=%d, want success on the 3rd attempt", err, r.calls.Load())
	}

	perm := &logsReceiver{failFirst: 5, failCode: codes.InvalidArgument}
	c = newTestClient(t, Config{Endpoint: startGRPCReceiver(t, perm), Insecure: true})
	if err := c.Send(context.Background(), sampleEvent); err == nil || perm.calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d, want one attempt for a permanent error", err, perm.calls.Load())
	}
}

func TestClient_HTTPWithGzipAndHeaders(t *testing.T) {
	r := &logsReceiver{}
	var got http.Header
	srv := httptest.NewServer(httpHandler(r, &got))
	t.Cleanup(srv.Close)
	c := newTestClient(t, Config{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Protocol: ProtocolHTTP,
		Insecure: true, Gzip: true, Headers: map[string]string{"X-Api-Key": "k"}})
	if err := c.Send(context.Background(), sampleEvent); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(r.records()) != 1 || got.Get("Content-Encoding") != "gzip" || got.Get("X-Api-Key") != "k" {
		t.Fatalf("records=%d headers=%v", len(r.records()), got)
	}
}

func TestClient_HTTPSWithCAFile(t *testing.T) {
	r := &logsReceiver{}
	var got http.Header
	srv := httptest.NewTLSServer(httpHandler(r, &got))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, Config{Endpoint: strings.TrimPrefix(srv.URL, "https://"), Protocol: ProtocolHTTP, CAFile: ca})
	if err := c.Send(context.Background(), sampleEvent); err != nil {
		t.Fatalf("TLS Send with CAFile: %v", err)
	}

	untrusted := newTestClient(t, Config{Endpoint: strings.TrimPrefix(srv.URL, "https://"), Protocol: ProtocolHTTP, MaxAttempts: 1})
	if err := untrusted.Send(context.Background(), sampleEvent); err == nil {
		t.Fatal("a server not signed by a trusted CA must fail")
	}
}

func TestClient_CloseThenSend(t *testing.T) {
	c := newTestClient(t, Config{Endpoint: startGRPCReceiver(t, &logsReceiver{}), Insecure: true})
	_ = c.Close(context.Background())
	if err := c.Send(context.Background(), sampleEvent); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}
