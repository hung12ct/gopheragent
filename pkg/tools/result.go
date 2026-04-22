package tools

import "github.com/hung12ct/gopheragent/pkg/history"

// Result is the return value of Tool.Execute.
//
// Text is what the LLM sees in the tool_result message of the conversation
// — the only field that round-trips to the model. Tools that only return
// text populate Text alone.
//
// UI is an optional structured payload surfaced to streaming clients. The
// agent loop JSON-encodes it into a PartialEvent or tool-complete event so
// the frontend can render a widget without double-parsing Text. When nil,
// no UI event is emitted.
//
// Parts optionally carries multi-modal content (currently image parts) for
// providers that accept non-text tool output. The Anthropic adapter is the
// only provider today that round-trips these; other adapters ignore Parts
// and fall back to Text.
type Result struct {
	Text  string
	UI    any
	Parts []history.MediaPart
}

// Emitter is passed to Tool.Execute and lets the tool stream partial
// payloads to the client while the tool is still running. The agent loop
// forwards each Partial call as a PartialEvent stream event carrying the
// ToolCallID binding it to the invocation.
//
// Tools that do not need progress streaming just ignore emit. The loop
// passes a no-op emitter (never nil) so implementations never have to
// nil-check.
type Emitter interface {
	// Partial emits one fragment of progress. payload may be any
	// JSON-marshalable value; the loop encodes it into the stream event.
	// Returning an error from Partial is informational — the loop logs it
	// but does not fail the tool invocation.
	Partial(payload any) error
}

// noopEmitter is the zero-cost Emitter used when no consumer is listening
// or when a tool is invoked directly outside an agent loop. The loop hands
// every Execute call this singleton when no stream is attached, so tools
// never see a nil Emitter.
type noopEmitter struct{}

// Partial discards the payload and returns nil. Safe for concurrent use.
func (noopEmitter) Partial(any) error { return nil }

// noopEmitterSingleton is the shared no-op emitter instance — a single
// value reused for every tool invocation that has no consumer, keeping
// the hot path allocation-free.
var noopEmitterSingleton Emitter = noopEmitter{}

// NoopEmitter returns the shared no-op Emitter singleton. Useful when
// calling Tool.Execute directly from outside an agent loop (e.g. in a
// unit test that only cares about Result.Text).
func NoopEmitter() Emitter { return noopEmitterSingleton }
