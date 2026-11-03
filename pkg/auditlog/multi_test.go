package auditlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
)

func TestMulti(t *testing.T) {
	ok, down := &fakeEmitter{}, &fakeEmitter{err: errors.New("down")}
	if err := Multi(down, ok).Send(actorCtx(), sampleEvent); err != nil || len(ok.events) != 1 {
		t.Fatalf("Multi with one healthy destination: err=%v delivered=%d", err, len(ok.events))
	}
	if err := Multi(down, &fakeEmitter{err: errors.New("also down")}).Send(actorCtx(), sampleEvent); err == nil || !strings.Contains(err.Error(), "2 of 2") {
		t.Fatalf("all destinations down must fail: %v", err)
	}
	if err := MultiAll(down, ok).Send(actorCtx(), sampleEvent); err == nil {
		t.Fatal("MultiAll must fail when any destination fails")
	}
	keyed := &fakeEmitter{key: testKey}
	m := Multi(&fakeEmitter{}, keyed)
	if fp := m.(Fingerprinter).Fingerprint("x"); fp != Hash(testKey, "x") {
		t.Fatalf("Multi fingerprint = %q", fp)
	}
	if err := m.(SyncEmitter).SendSync(actorCtx(), sampleEvent); err != nil || keyed.syncs != 1 {
		t.Fatalf("Multi.SendSync must use each destination's SendSync: err=%v syncs=%d", err, keyed.syncs)
	}
}

func TestJSONLWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NewJSONLWriter(&buf)
	if err := w.Send(context.Background(), sampleEvent, ApprovalDecision{Common: Common{TenantID: "a"}, Tool: "t", Decision: DecisionApproved}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", buf.String())
	}
	var line struct {
		EventName string         `json:"event_name"`
		Event     map[string]any `json:"event"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil || line.EventName != "gopheragent.audit.DataModification.v1" || line.Event["tenant_id"] != "acme" {
		t.Fatalf("line = %s (%v)", lines[0], err)
	}
	if err := w.Send(context.Background(), ToolInvocation{}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatal("JSONLWriter must validate")
	}
}

func TestSelfMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t")

	good := newTestClient(t, Config{Endpoint: startGRPCReceiver(t, &logsReceiver{}), Insecure: true, Meter: meter})
	_ = good.Send(context.Background(), sampleEvent, sampleEvent)
	_ = good.Send(context.Background(), ToolInvocation{Tool: "x", Outcome: OutcomeSuccess})
	bad := newTestClient(t, Config{Endpoint: startGRPCReceiver(t, &logsReceiver{failFirst: 9, failCode: codes.PermissionDenied}), Insecure: true, Meter: meter})
	_ = bad.Send(context.Background(), sampleEvent)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	var sawDuration bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					k, _ := dp.Attributes.Value(attribute.Key("kind"))
					r, _ := dp.Attributes.Value(attribute.Key("result"))
					got[k.AsString()+"/"+r.AsString()] += dp.Value
				}
			case metricdata.Histogram[float64]:
				sawDuration = m.Name == "gopheragent.audit.export.duration"
			}
		}
	}
	want := map[string]int64{"DataModification/sent": 2, "ToolInvocation/invalid": 1, "DataModification/failed": 1}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("records %s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	if !sawDuration {
		t.Fatal("export duration histogram not recorded")
	}
}
