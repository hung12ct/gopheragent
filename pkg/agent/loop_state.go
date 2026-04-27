package agent

import (
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
