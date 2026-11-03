package auditlog

import (
	"context"
	"fmt"
	"time"
)

// BatchConfig tunes ModeBatch.
type BatchConfig struct {
	// BufferSize is the queue capacity; beyond it Send drops and returns
	// ErrBufferFull. Default 10000.
	BufferSize int
	// MaxBatch is the most records per export request. Default 400.
	MaxBatch int
	// MaxWait is how long a partial batch waits before export. Default 1s.
	MaxWait time.Duration
}

func (b *BatchConfig) applyDefaults() {
	if b.BufferSize <= 0 {
		b.BufferSize = 10000
	}
	if b.MaxBatch <= 0 {
		b.MaxBatch = 400
	}
	if b.MaxWait <= 0 {
		b.MaxWait = time.Second
	}
}

// batcher is ModeBatch's single background worker. One worker keeps records
// in sequence order; throughput comes from batching, not parallel exports.
type batcher struct {
	c       *Client
	cfg     BatchConfig
	queue   chan item
	flushes chan chan struct{}
	stop    chan struct{}
	done    chan struct{}

	pending []item // worker-owned
	timer   *time.Timer
}

func newBatcher(c *Client, cfg BatchConfig) *batcher {
	b := &batcher{
		c: c, cfg: cfg,
		queue:   make(chan item, cfg.BufferSize),
		flushes: make(chan chan struct{}),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		pending: make([]item, 0, cfg.MaxBatch),
		timer:   time.NewTimer(cfg.MaxWait),
	}
	b.timer.Stop()
	go b.run()
	return b
}

// enqueue never blocks: an event that doesn't fit is dropped, counted, and
// reported to the caller.
func (b *batcher) enqueue(ctx context.Context, items []item) error {
	var dropped []item
	for _, it := range items {
		select {
		case b.queue <- it:
		default:
			dropped = append(dropped, it)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	b.c.metrics.countItems(ctx, dropped, resultDropped)
	return fmt.Errorf("%w: dropped %d of %d event(s)", ErrBufferFull, len(dropped), len(items))
}

func (b *batcher) run() {
	defer close(b.done)
	for {
		select {
		case it := <-b.queue:
			b.add(it)
		case <-b.timer.C:
			b.export()
		case ack := <-b.flushes:
			b.drain()
			close(ack)
		case <-b.stop:
			b.drain()
			return
		}
	}
}

func (b *batcher) add(it item) {
	b.pending = append(b.pending, it)
	if len(b.pending) == 1 {
		b.timer.Reset(b.cfg.MaxWait)
	}
	if len(b.pending) >= b.cfg.MaxBatch {
		b.export()
	}
}

// drain exports everything queued right now, then the partial batch.
func (b *batcher) drain() {
	for {
		select {
		case it := <-b.queue:
			b.add(it)
		default:
			b.export()
			return
		}
	}
}

func (b *batcher) export() {
	b.timer.Stop()
	if len(b.pending) == 0 {
		return
	}
	ctx := context.Background()
	if err := b.c.sendItems(ctx, b.pending); err != nil {
		b.c.cfg.OnError(ctx, fmt.Errorf("auditlog: batch of %d: %w", len(b.pending), err))
	}
	clear(b.pending) // drop event references for the GC
	b.pending = b.pending[:0]
}

func (b *batcher) flush(ctx context.Context) error {
	ack := make(chan struct{})
	select {
	case b.flushes <- ack:
	case <-b.done:
		return ErrClosed
	case <-ctx.Done():
		return fmt.Errorf("auditlog: flush: %w", ctx.Err())
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("auditlog: flush: %w", ctx.Err())
	}
}

func (b *batcher) close(ctx context.Context) error {
	close(b.stop)
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("auditlog: close: draining batch buffer: %w", ctx.Err())
	}
}
