package agent

import (
	"context"
	"strings"
)

// streamDrainer reads provider stream events, accumulates content,
// dispatches speculation for ready tool calls, and forwards every event
// to the parent stream. The four jobs that lived inline in the old
// callLLM closure are now named methods on this type.
//
// Allocation profile matches the original: one heap object per LLM
// call. Single-goroutine use — drain owns the receive loop.
type streamDrainer struct {
	al      *AgentLoop
	st      *iterationState
	ctx     context.Context
	buf     strings.Builder
	emitted bool
}

// newStreamDrainer constructs a drainer for one LLM call.
func newStreamDrainer(al *AgentLoop, st *iterationState, ctx context.Context) *streamDrainer {
	return &streamDrainer{al: al, st: st, ctx: ctx}
}

// drain reads pChan to completion, returning when ctx cancels or the
// channel closes. Closes done so the caller can synchronise on a
// finished drainer before reading buf/emitted.
func (d *streamDrainer) drain(pChan <-chan StreamEvent, done chan<- struct{}) {
	defer close(done)
	for ev := range pChan {
		if !d.handleEvent(ev) {
			// ctx cancelled; drain remaining events to unblock producer.
			for range pChan {
			}
			return
		}
	}
}

// handleEvent processes one event and forwards it. Returns false when
// ctx has cancelled and the caller should stop forwarding.
func (d *streamDrainer) handleEvent(ev StreamEvent) bool {
	if ev.Type == EventTypeContent {
		d.buf.WriteString(ev.Content)
		d.emitted = true
	}
	d.maybeSpeculate(ev)
	select {
	case d.st.streamChan <- ev:
		for _, h := range d.al.EventHandlers {
			safeCallHandler(h, d.ctx, d.st.sessionKey, ev)
		}
		return true
	case <-d.ctx.Done():
		return false
	}
}

// maybeSpeculate dispatches spawnSpeculative for eligible
// tool_call_ready events. Pure dispatch; never blocks.
func (d *streamDrainer) maybeSpeculate(ev StreamEvent) {
	if ev.Type != EventTypeToolCallReady {
		return
	}
	p, ok := ev.Payload().(ToolCallReadyEvent)
	if !ok {
		return
	}
	if !d.al.shouldSpeculate(d.st.sessionKey, p.ID, p.Name, p.ArgsJSON) {
		return
	}
	d.al.spawnSpeculative(d.ctx, p.ID, p.Name, p.ArgsJSON, d.st.specMu, d.st.specMap)
}

// content returns the accumulated content text. Safe to call after
// drain has signalled done.
func (d *streamDrainer) content() string { return d.buf.String() }

// contentEmitted reports whether at least one content event was
// observed (used to decide retry safety: retries with content already
// streamed cannot be retried without confusing the caller).
func (d *streamDrainer) contentEmitted() bool { return d.emitted }
