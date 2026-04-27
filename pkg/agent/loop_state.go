package agent

import (
	"fmt"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// iterationState carries the per-iteration data shared across the
// callLLM and tool-execution methods. The fields collapse the closure-
// captured variables that used to live inside runLogicLoop into one
// addressable struct so each extracted method can take a single
// *iterationState pointer instead of a forest of separate args.
type iterationState struct {
	sessionKey string
	iteration  int
	streamChan chan<- StreamEvent
	msgs       *[]history.Message
	specMap    map[string]*speculativeExec
	specMu     *sync.Mutex
	tracker    *LoopDetector
}

// waveState carries the per-wave shared mutable state used by the
// goroutines that execute tool calls in parallel. Always passed as
// *waveState so the embedded mutexes are addressable.
type waveState struct {
	toolMsgs    map[string]history.Message
	resultsByID map[string]string
	completedMu sync.Mutex
	fatalErr    error
	fatalMu     sync.Mutex
	hitlMu      sync.Mutex
}

// newWaveState returns an initialised waveState with maps sized for the
// expected per-wave call count.
func newWaveState(capacity int) *waveState {
	return &waveState{
		toolMsgs:    make(map[string]history.Message, capacity),
		resultsByID: make(map[string]string, capacity),
	}
}

// recordToolMsg writes a tool result message under completedMu and,
// when isResult is true, also publishes the content to resultsByID so
// downstream <output_of:...> substitutions can find it. Centralising
// this triple removes a class of "did I forget to update resultsByID"
// bugs at the call sites.
func (ws *waveState) recordToolMsg(id string, msg history.Message, isResult bool) {
	ws.completedMu.Lock()
	ws.toolMsgs[id] = msg
	if isResult {
		ws.resultsByID[id] = msg.Content
	}
	ws.completedMu.Unlock()
}

// setFatal records the first fatal error seen by any wave goroutine.
// First-writer-wins: subsequent calls are no-ops.
func (ws *waveState) setFatal(err error) {
	ws.fatalMu.Lock()
	if ws.fatalErr == nil {
		ws.fatalErr = err
	}
	ws.fatalMu.Unlock()
}

// appendAssistantToolCallMsg builds the assistant message that records
// the LLM's tool-call request and appends it to msgs. Pure: the caller
// supplies content and calls; the function performs no I/O.
func appendAssistantToolCallMsg(msgs []history.Message, content string, calls []PendingToolCall) []history.Message {
	m := history.Message{Role: "assistant", Content: content}
	for _, tc := range calls {
		m.ToolCalls = append(m.ToolCalls, history.ToolCall{
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: tc.ArgsJSON,
		})
	}
	return append(msgs, m)
}

// appendToolResultsInOrder drains toolMsgs in the original LLM call
// order and appends present results to msgs. Calls with no matching
// entry are skipped silently.
func appendToolResultsInOrder(msgs []history.Message, calls []PendingToolCall, toolMsgs map[string]history.Message) []history.Message {
	for _, tc := range calls {
		if m, ok := toolMsgs[tc.ID]; ok && m.Role != "" {
			msgs = append(msgs, m)
		}
	}
	return msgs
}

// synthesizeDroppedToolErrors writes a tool-error message for each
// dropped tool call so the drain loop emits them in their original
// position and the model reads a clear reason next turn. Caller writes
// directly into the per-wave map; no lock is needed because every wave
// goroutine has returned by the time this runs.
func synthesizeDroppedToolErrors(toolMsgs map[string]history.Message, dropped []PendingToolCall, max int) {
	for _, tc := range dropped {
		toolMsgs[tc.ID] = history.Message{
			Role:       "tool",
			Content:    fmt.Sprintf("tools: dropped by per-turn tool-call budget (max=%d); retry fewer calls next turn.", max),
			ToolCallID: tc.ID,
			IsError:    true,
		}
	}
}
