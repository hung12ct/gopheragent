package agent

import (
	"context"
	"strings"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// speculativeExec carries the in-flight or completed result of a tool that
// was kicked off before the LLM finished streaming. The doneCh is closed
// when execution returns; readers receive the cached (result, err) pair
// without racing the writer goroutine.
//
// A single instance is created per tool call ID. Entries are inserted by
// the stream drainer (single writer) and read by the wave executor only
// after the drainer signals completion — no concurrent map access.
type speculativeExec struct {
	id     string
	doneCh chan struct{}
	result string
	err    error
}

// newSpeculativeMap returns an initialized map keyed by tool call ID.
// Kept as a helper so the tests can construct one without touching the
// internal zero-value rules.
func newSpeculativeMap() map[string]*speculativeExec {
	return make(map[string]*speculativeExec)
}

// shouldSpeculate reports whether a ready tool call is safe to execute
// before the rest of the LLM response arrives. The checks are conservative
// because a mistaken speculation has real cost: running a HITL-gated tool
// without approval, running a tool whose arguments refer to a later call's
// output (via <output_of:...>), or running a tool at all while PlanMode is
// waiting for user approval are all correctness bugs, not just efficiency
// ones. When in doubt, skip — the wave executor will run it normally.
// id is unused today but kept in the signature so future eligibility
// checks (e.g. de-dup against prior failed speculations) can reference it
// without churning every call site.
func (al *AgentLoop) shouldSpeculate(sessionKey, _, name, argsJSON string) bool {
	if !al.SpeculativeTools {
		return false
	}
	if al.IsPlanMode(sessionKey) {
		return false
	}
	if name == ExitPlanModeToolName {
		// exit_plan_mode is a sentinel handled inline by the loop; never
		// execute it as a real tool, speculatively or otherwise.
		return false
	}
	if al.Tools == nil {
		return false
	}
	tool, ok := al.Tools.Get(name)
	if !ok {
		return false
	}
	if tool.RequiresConfirmation() {
		return false
	}
	// The scheduler resolves <output_of:ID.path> tokens only after every
	// tool call is known, so a speculated call that references a later
	// one would run against unsubstituted arguments and fail.
	if strings.Contains(argsJSON, "<output_of:") {
		return false
	}
	return true
}

// spawnSpeculative kicks off a background execution of tool with argsJSON.
// The returned *speculativeExec is stored in the map keyed by id so the
// wave executor can reuse the result once the full LLM response arrives.
//
// The execution runs without progress-event plumbing (speculative tools
// have no reader at this point) and without cache bookkeeping; the wave
// executor applies the regular cache.Put on the result when it processes
// the call, keeping a single source of truth for caching policy.
func (al *AgentLoop) spawnSpeculative(
	ctx context.Context,
	id, name, argsJSON string,
	mu *sync.Mutex,
	store map[string]*speculativeExec,
) {
	sm := &speculativeExec{
		id:     id,
		doneCh: make(chan struct{}),
	}
	mu.Lock()
	if _, exists := store[id]; exists {
		// Duplicate ready-event for the same call (shouldn't happen, but
		// cheap to guard against) — skip silently.
		mu.Unlock()
		return
	}
	store[id] = sm
	mu.Unlock()

	tool, ok := al.Tools.Get(name)
	if !ok {
		sm.err = &ToolNotFoundError{ToolName: name}
		close(sm.doneCh)
		return
	}

	go func() {
		defer close(sm.doneCh)
		// Run with a bare ctx — no progress reporter, no sub-agent
		// emitter. The wave executor owns user-visible emissions for this
		// call when it processes the result.
		toolCtx := ctx
		// Cast through tools.Tool interface to avoid importing internals.
		var t tools.Tool = tool
		result, err := t.Execute(toolCtx, argsJSON)
		sm.result = result
		sm.err = err
	}()
}

// awaitSpeculative blocks until the speculative execution completes and
// returns its (result, err). Safe to call from the wave executor after the
// LLM stream has closed; doneCh acts as the happens-before barrier.
func awaitSpeculative(ctx context.Context, sm *speculativeExec) (string, error) {
	select {
	case <-sm.doneCh:
		return sm.result, sm.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
