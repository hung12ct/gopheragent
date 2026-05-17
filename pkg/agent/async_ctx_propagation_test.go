package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// ctxValueCapturingProvider records ctx.Value(testCtxKey{}) on each LLM call
// so tests can assert that values stamped on the parent ctx survive into
// the async worker.
type ctxValueCapturingProvider struct {
	mu     sync.Mutex
	seen   []any
	called chan struct{}
}

type testCtxKey struct{}

func (p *ctxValueCapturingProvider) GenerateStream(ctx context.Context, _ []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.mu.Lock()
	p.seen = append(p.seen, ctx.Value(testCtxKey{}))
	p.mu.Unlock()
	if p.called != nil {
		select {
		case p.called <- struct{}{}:
		default:
		}
	}
	ch <- Event(ContentEvent{Text: "done"})
	return LLMResult{Content: "done"}, nil
}

// TestAsyncTaskManager_PropagatesParentCtxValues locks in the contract that
// values stamped on parentCtx (user_id, request_id, tracer spans, etc.)
// flow into the worker. The pre-fix implementation passed
// context.Background() to the worker, dropping every value the caller
// installed and breaking middleware-driven systems.
func TestAsyncTaskManager_PropagatesParentCtxValues(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	prov := &ctxValueCapturingProvider{called: make(chan struct{}, 1)}
	mgr := NewAsyncTaskManager(sm, reg, prov)

	parentCtx := context.WithValue(context.Background(), testCtxKey{}, "tenant-42")
	if _, err := mgr.StartTask(parentCtx, "parent", "worker", "do something"); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	select {
	case <-prov.called:
	case <-time.After(2 * time.Second):
		t.Fatal("worker provider was never invoked")
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.seen) == 0 {
		t.Fatal("provider recorded no ctx-value samples")
	}
	got := prov.seen[0]
	if got != "tenant-42" {
		t.Fatalf("worker ctx lost parent value — got %v, want %q", got, "tenant-42")
	}
}

// TestAsyncTaskManager_ParentCancelDoesNotKillWorker locks in the second
// half of the contract: the worker is detached from parentCtx's Done
// channel so that an SSE request closing (or any caller cancellation)
// does not abort an in-flight long-running tool (Veo poll, image
// generation, etc.).
func TestAsyncTaskManager_ParentCancelDoesNotKillWorker(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	reg := tools.NewRegistry()
	prov := &ctxValueCapturingProvider{called: make(chan struct{}, 1)}
	mgr := NewAsyncTaskManager(sm, reg, prov)

	parentCtx, cancel := context.WithCancel(context.Background())
	if _, err := mgr.StartTask(parentCtx, "parent", "worker", "do something"); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	cancel() // kill the parent ctx immediately

	select {
	case <-prov.called:
		// Worker still ran to completion despite the parent cancel — the
		// async-detachment invariant holds.
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not run after parent cancellation — async-detachment invariant broken")
	}
}
