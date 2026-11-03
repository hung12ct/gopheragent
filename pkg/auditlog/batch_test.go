package auditlog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

func batchClient(t *testing.T, r *logsReceiver, b BatchConfig) *Client {
	t.Helper()
	return newTestClient(t, Config{Endpoint: startGRPCReceiver(t, r), Insecure: true, HMACKey: testKey, Mode: ModeBatch, Batch: b})
}

func TestBatch_BatchesAndKeepsOrder(t *testing.T) {
	r := &logsReceiver{}
	c := batchClient(t, r, BatchConfig{MaxBatch: 100, MaxWait: time.Hour})
	for i := 0; i < 1000; i++ {
		if err := c.Send(context.Background(), sampleEvent); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var recs []Record
	for _, lr := range r.records() {
		recs = append(recs, RecordFromOTLP(lr))
	}
	if len(recs) != 1000 || r.calls.Load() > 11 {
		t.Fatalf("records=%d requests=%d, want 1000 records in ~10 requests", len(recs), r.calls.Load())
	}
	if rep := Verify(testKey, recs); !rep.OK() {
		t.Fatalf("batched trail not contiguous/signed: %+v", rep)
	}
}

func TestBatch_MaxWaitExportsPartialBatch(t *testing.T) {
	r := &logsReceiver{}
	c := batchClient(t, r, BatchConfig{MaxBatch: 100, MaxWait: 20 * time.Millisecond})
	_ = c.Send(context.Background(), sampleEvent)
	deadline := time.Now().Add(2 * time.Second)
	for len(r.records()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(r.records()) != 1 {
		t.Fatal("a partial batch must export after MaxWait")
	}
}

func TestBatch_FullBufferDropsAndReports(t *testing.T) {
	r := &logsReceiver{gate: make(chan struct{})}
	c := batchClient(t, r, BatchConfig{BufferSize: 1, MaxBatch: 1, MaxWait: time.Hour})
	_ = c.Send(context.Background(), sampleEvent) // worker takes it and stalls in export
	deadline := time.Now().Add(2 * time.Second)
	var err error
	for time.Now().Before(deadline) { // fill the 1-slot buffer, then overflow
		if err = c.Send(context.Background(), sampleEvent); errors.Is(err, ErrBufferFull) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(err, ErrBufferFull) {
		t.Fatalf("err = %v, want ErrBufferFull", err)
	}
	close(r.gate)
}

func TestBatch_PreservesPerEventTraceContext(t *testing.T) {
	r := &logsReceiver{}
	c := batchClient(t, r, BatchConfig{MaxWait: time.Hour})
	ids := []string{"11111111111111111111111111111111", "22222222222222222222222222222222"}
	for _, h := range ids {
		tid, _ := trace.TraceIDFromHex(h)
		sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
		ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid}))
		_ = c.Send(ctx, sampleEvent)
	}
	_ = c.Flush(context.Background())
	recs := r.records()
	for i, h := range ids {
		tid, _ := trace.TraceIDFromHex(h)
		if len(recs) != 2 || string(recs[i].GetTraceId()) != string(tid[:]) {
			t.Fatalf("record %d lost its own trace context in the batch", i)
		}
	}
}

func TestBatch_CloseDrainsAndInvalidIsSynchronous(t *testing.T) {
	r := &logsReceiver{}
	c := batchClient(t, r, BatchConfig{MaxWait: time.Hour})
	if err := c.Send(context.Background(), ToolInvocation{Tool: "x", Outcome: OutcomeSuccess}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("batch mode must still reject invalid events at Send: %v", err)
	}
	for i := 0; i < 5; i++ {
		_ = c.Send(context.Background(), sampleEvent)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.records()) != 5 {
		t.Fatalf("Close drained %d of 5 queued records", len(r.records()))
	}
	if err := c.Send(context.Background(), sampleEvent); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close = %v", err)
	}
}

func TestBatch_ConcurrentSendAndClose(t *testing.T) {
	r := &logsReceiver{}
	c := batchClient(t, r, BatchConfig{MaxBatch: 16, MaxWait: time.Millisecond})
	var wg sync.WaitGroup
	var accepted sync.Map
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if err := c.Send(context.Background(), sampleEvent); err == nil {
					n, _ := accepted.LoadOrStore(g, new(int))
					*n.(*int)++
				}
			}
		}()
	}
	wg.Wait()
	_ = c.Close(context.Background())
	total := 0
	accepted.Range(func(_, v any) bool { total += *v.(*int); return true })
	if len(r.records()) != total {
		t.Fatalf("exported %d, accepted %d: every accepted event must be delivered", len(r.records()), total)
	}
}

func TestBatch_FailClosedWritesStartedSynchronously(t *testing.T) {
	r := &logsReceiver{}
	c := batchClient(t, r, BatchConfig{MaxWait: time.Hour})
	tool := tools.Chain(&stubTool{name: "wire"}, ToolMiddleware(c, WithFailClosed()))
	if _, err := tool.Execute(actorCtx(), "{}"); err != nil {
		t.Fatal(err)
	}
	recs := r.records() // no Flush: only the synchronous record can be here
	if len(recs) != 1 || !containsStr(recs[0].GetBody().GetStringValue(), `"outcome":"started"`) {
		t.Fatalf("started record must be written before the tool runs, even in batch mode (got %d records)", len(recs))
	}
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }
