package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// ToolDegradation is one tool call that half-succeeded, as reported by a
// tools.Degradation and stamped with the tool that raised it. See
// tools.Degradation for the field semantics.
type ToolDegradation struct {
	Tool       string   `json:"tool"`
	Reason     string   `json:"reason"`
	Artifacts  []string `json:"artifacts,omitempty"`
	Unreliable []string `json:"unreliable,omitempty"`
}

// DegradedError is the error form of a partial-success terminal, for
// adopters that classify turns by error rather than by event type. It
// matches errors.Is(err, ErrDegraded).
//
// It is never returned from Run — a degraded turn produced a real answer,
// and surfacing it as a returned error would push callers to discard
// work that landed. It reaches adopters through DegradedEvent.Err.
type DegradedError struct {
	Units []ToolDegradation
}

func (e *DegradedError) Error() string {
	names := make([]string, 0, len(e.Units))
	for _, u := range e.Units {
		names = append(names, u.Tool)
	}
	return fmt.Sprintf("agent: turn completed with degraded state from %s", strings.Join(names, ", "))
}

// Is reports ErrDegraded so errors.Is matches without unwrapping.
func (e *DegradedError) Is(target error) bool { return target == ErrDegraded }

// degradedKey is the ctx-value key for the per-Run degradation accumulator.
type degradedKey struct{}

// degradedAcc collects the degradations raised by tools across every
// iteration of one Run. The mutex is load-bearing: tool waves execute in
// parallel goroutines, so several tools can degrade at once.
type degradedAcc struct {
	mu    sync.Mutex
	units []ToolDegradation
}

func (a *degradedAcc) add(u ToolDegradation) {
	a.mu.Lock()
	a.units = append(a.units, u)
	a.mu.Unlock()
}

// drain returns the accumulated degradations and clears the accumulator,
// so whichever terminal path fires first reports them and later paths
// (the Run-level deferred sweep) do not emit a duplicate.
func (a *degradedAcc) drain() []ToolDegradation {
	a.mu.Lock()
	defer a.mu.Unlock()
	units := a.units
	a.units = nil
	return units
}

func degradedAccFromContext(ctx context.Context) *degradedAcc {
	v, _ := ctx.Value(degradedKey{}).(*degradedAcc)
	return v
}

// installDegradationAccumulator stashes a fresh accumulator on ctx and
// returns it alongside a sweep callback that emits any degradation no
// terminal path has claimed yet.
//
// Unlike installRunCostAccumulator, which skips the ctx allocation
// entirely when PriceTable is nil, this one always allocates: whether a
// tool will degrade is not knowable up front and there is no config knob
// to gate on. The cost is one small struct, one ctx value, and one
// closure per Run — deliberate, not an oversight.
//
// Caller pattern:
//
//	ctx, sweepDegraded := al.installDegradationAccumulator(ctx, sessionKey, streamChan)
//	defer sweepDegraded()
//
// The sweep exists so a Run that degrades and then dies on MaxIters or a
// fatal LLM error still reports which state went unreliable — that is
// exactly the run whose bookkeeping most needs repairing.
func (al *AgentLoop) installDegradationAccumulator(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent) (context.Context, func()) {
	ctx = context.WithValue(ctx, degradedKey{}, &degradedAcc{})
	return ctx, func() {
		al.emitDegradedIfAny(ctx, sessionKey, streamChan)
	}
}

// recordDegradation files a tool's partial-success report against the
// Run's accumulator. No-op when called outside a Run that installed one
// (sub-agent tools invoked directly in tests, for example).
func recordDegradation(ctx context.Context, toolName string, d *tools.Degradation) {
	// Check d first: the speculative path calls this on every successful
	// execution, and the overwhelming majority pass nil. Testing the
	// pointer before walking the ctx chain keeps the common case free.
	if d == nil {
		return
	}
	acc := degradedAccFromContext(ctx)
	if acc == nil {
		return
	}
	acc.add(ToolDegradation{
		Tool:       toolName,
		Reason:     d.Reason,
		Artifacts:  d.Artifacts,
		Unreliable: d.Unreliable,
	})
}

// applyDegradation annotates a half-succeeded tool result so the model
// does not redo the half that landed, and files the report against the
// Run's accumulator. Returns result unchanged when the call did not
// degrade, or when it degraded into an outright error — there the error
// is the story and the failure path already tells it.
//
// A speculated result is annotated but not filed: spawnSpeculative
// reports at execution time instead, so a speculation the loop later
// discards (retry reset, stream error) still reaches the host. Filing
// again here would double-count it.
func applyDegradation(ctx context.Context, toolName, result string, d *tools.Degradation, execErr error, speculated bool) string {
	if d == nil || execErr != nil {
		return result
	}
	if !speculated {
		recordDegradation(ctx, toolName, d)
	}
	return result + degradationNote(d)
}

// emitDegradedIfAny drains the accumulator and emits a DegradedEvent when
// anything degraded this Run. Called immediately before DoneEvent on the
// final-answer path, and again from the Run-level defer to catch turns
// that ended on a cap or a fatal error.
func (al *AgentLoop) emitDegradedIfAny(ctx context.Context, sessionKey string, streamChan chan<- StreamEvent) {
	acc := degradedAccFromContext(ctx)
	if acc == nil {
		return
	}
	units := acc.drain()
	if len(units) == 0 {
		return
	}
	al.emit(ctx, sessionKey, streamChan, Event(DegradedEvent{
		Units: units,
		Err:   &DegradedError{Units: units},
	}))
}

// degradationNote renders the model-facing partial-success annotation
// appended to a degraded tool's result. It tells the model not to redo
// the work that landed, which is the failure mode a bare error would
// cause.
func degradationNote(d *tools.Degradation) string {
	reason := d.Reason
	if reason == "" {
		reason = "the tool reported that part of its work did not complete"
	}
	var b strings.Builder
	b.WriteString("\n\n[System: partial success] ")
	b.WriteString(reason)
	if len(d.Artifacts) > 0 {
		b.WriteString("\nLanded and must NOT be retried or discarded: ")
		b.WriteString(strings.Join(d.Artifacts, ", "))
	}
	if len(d.Unreliable) > 0 {
		b.WriteString("\nNow unreliable — treat as suspect and repair before relying on it: ")
		b.WriteString(strings.Join(d.Unreliable, ", "))
	}
	return b.String()
}
