package auditlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Multi fans events out to several emitters in parallel and succeeds when at
// least one of them does — keep two independent destinations (say the
// collector and a local JSONLWriter) so one outage doesn't lose the trail.
// It fails only when every emitter fails.
func Multi(emitters ...Emitter) Emitter { return &multi{emitters: emitters} }

// MultiAll is Multi that requires every emitter to succeed.
func MultiAll(emitters ...Emitter) Emitter { return &multi{emitters: emitters, all: true} }

type multi struct {
	emitters []Emitter
	all      bool
}

var (
	_ SyncEmitter   = (*multi)(nil)
	_ Fingerprinter = (*multi)(nil)
)

func (m *multi) Send(ctx context.Context, events ...Event) error {
	return m.fanOut(func(e Emitter) error { return e.Send(ctx, events...) })
}

// SendSync bypasses buffering on every emitter that supports it.
func (m *multi) SendSync(ctx context.Context, events ...Event) error {
	return m.fanOut(func(e Emitter) error {
		if s, ok := e.(SyncEmitter); ok {
			return s.SendSync(ctx, events...)
		}
		return e.Send(ctx, events...)
	})
}

// Fingerprint delegates to the first emitter holding a key.
func (m *multi) Fingerprint(value string) string {
	for _, e := range m.emitters {
		if f, ok := e.(Fingerprinter); ok {
			if fp := f.Fingerprint(value); fp != "" {
				return fp
			}
		}
	}
	return ""
}

func (m *multi) fanOut(send func(Emitter) error) error {
	errs := make([]error, len(m.emitters))
	var wg sync.WaitGroup
	for i, e := range m.emitters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = send(e)
		}()
	}
	wg.Wait()
	failed := 0
	for _, err := range errs {
		if err != nil {
			failed++
		}
	}
	if failed == 0 || (!m.all && failed < len(m.emitters)) {
		return nil
	}
	return fmt.Errorf("auditlog: %d of %d destinations failed: %w", failed, len(m.emitters), errors.Join(errs...))
}

// JSONLWriter writes each event as one JSON line to w — a local audit device
// for development or as Multi's second destination. Writes are serialized;
// rotation and fsync policy belong to w.
type JSONLWriter struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// NewJSONLWriter returns a JSONLWriter on w.
func NewJSONLWriter(w io.Writer) *JSONLWriter { return &JSONLWriter{w: w, now: time.Now} }

// Send validates every event, then writes one line per event: the same body
// the OTLP client sends, wrapped with its event name.
func (j *JSONLWriter) Send(_ context.Context, events ...Event) error {
	var verrs []error
	for _, e := range events {
		if e == nil {
			verrs = append(verrs, fmt.Errorf("%w: nil event", ErrInvalidEvent))
		} else if err := e.Validate(); err != nil {
			verrs = append(verrs, err)
		}
	}
	if err := errors.Join(verrs...); err != nil {
		return err
	}
	now := j.now()
	var buf []byte
	for _, e := range events {
		at := e.common().Time
		if at.IsZero() {
			at = now
		}
		body, err := eventJSON(e, at)
		if err != nil {
			return err
		}
		buf = append(buf, `{"event_name":"`+EventName(e)+`","event":`...)
		buf = append(append(buf, body...), "}\n"...)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.w.Write(buf); err != nil {
		return fmt.Errorf("auditlog: jsonl write: %w", err)
	}
	return nil
}
