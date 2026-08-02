// Package tools defines the Tool interface and a thread-safe Registry for managing agent tools.
package tools

import (
	"context"
	"log/slog"
	"maps"
	"sort"
	"sync"
)

// ToolSchema defines the JSON Schema for a tool's parameters.
// It maps directly to the OpenAI/Anthropic function calling schema format.
type ToolSchema struct {
	Type       string         `json:"type"`                 // typically "object"
	Properties map[string]any `json:"properties,omitempty"` // parameter definitions
	Required   []string       `json:"required,omitempty"`   // required parameter names
}

// ToolDescriptor carries the static metadata of a Tool. Returned by
// Tool.Descriptor() — every capability flag the loop reads lives here so
// wrappers preserve them for free (the previous design used opt-in
// method-set interfaces that dropped silently on wrap).
type ToolDescriptor struct {
	Name                 string
	Description          string
	Parameters           ToolSchema
	RequiresConfirmation bool
	// Cacheable is true when the tool's results are deterministic and safe
	// for the agent loop to cache by (name, argsJSON). Tools that hit live
	// data (prices, time, remote APIs without idempotency) leave this false.
	Cacheable bool
	// Inline is true when the tool's text output should be streamed to the
	// frontend as a content event in addition to being fed back to the LLM
	// as a tool result. Used by image/video tools whose result is a markdown
	// URL the FE needs to render directly.
	Inline bool
	// Display carries UI-facing metadata (label, description, category, icon
	// hint). Use DefaultDisplay(name, description) for tools without special
	// UI treatment.
	Display ToolDisplay
}

// Result is the return value of Tool.Execute.
//
// Text is what flows back to the LLM as the tool_result content — the only
// field that round-trips to the model. Tools that only return text populate
// Text alone (use Text() for a one-liner).
//
// Structured is an optional typed payload delivered to the agent loop's
// OnToolResult hook so post-execution mutators can work with fields instead
// of substring-replacing markdown. Nil when the tool has nothing structured
// to surface.
//
// Parts is optional multi-modal output for providers that accept non-text
// tool results. Empty when the tool returns text only.
//
// Degraded is set by a tool that half-succeeded — see Degradation. Nil for
// the overwhelming majority of calls; the loop pays one nil check.
type Result struct {
	Text       string
	Structured any
	Parts      []MediaPart
	Degraded   *Degradation
}

// String returns the result's model-facing text, so a Result formats
// sensibly with %s / %q / %v instead of dumping a struct with a raw
// pointer in it.
//
// Load-bearing beyond convenience: without a String method, `go vet`'s
// printf check rejects `%s`/`%q` on a Result because of the *Degradation
// field, and that failure lands in adopters' builds — including callers
// who never touch the degradation feature. Do not remove it without
// re-checking `go vet` in a module that only consumes this package.
//
// Deliberately renders Text alone. Structured, Parts, and Degraded are
// separate channels with their own consumers; folding them in here would
// make a debug line's output depend on which of them a tool happened to
// populate.
func (r Result) String() string { return r.Text }

// Degradation marks a tool call that half-succeeded: the expensive,
// durable part of the work landed but the derived bookkeeping did not.
// A tool that writes a file and then fails to update its index is the
// canonical case — returning an error would invite a retry that
// duplicates the write, and returning a clean Result would hide the
// inconsistency.
//
// A tool raises one by returning a normal (non-error) Result with this
// field set. The loop appends a short partial-success note to the text
// the model sees, and rolls every degradation of the turn into a
// terminal DegradedEvent for the host.
//
// Reason is a one-line human summary. Artifacts names what did land and
// must not be discarded or redone; Unreliable names the state a
// subsequent run should treat as suspect and repair. Both are opaque
// identifiers chosen by the tool (paths, IDs, table names).
type Degradation struct {
	Reason     string
	Artifacts  []string
	Unreliable []string
}

// MediaPart is a leaf-package multi-modal output unit. Tools that emit
// images or files populate this; the agent loop adapts to history.MediaPart
// at the boundary. Kept inline so pkg/tools stays a leaf package (no upward
// dep on pkg/history).
type MediaPart struct {
	MIMEType string
	Data     []byte
	Text     string
}

// Text is a convenience constructor for text-only results:
//
//	return tools.Text("ok"), nil
func Text(s string) Result { return Result{Text: s} }

// Tool is the minimal interface every agent tool must satisfy.
//
// Descriptor returns the static metadata block — name, description,
// parameters schema, capability flags. Wrappers preserve every capability
// flag trivially because Descriptor is one accessor returning a value
// struct (no method-set interface dance).
//
// Execute runs the tool with the given JSON arguments and returns a Result.
// Errors are returned alongside the zero Result so the loop can format the
// failure as a tool-error message.
type Tool interface {
	Descriptor() ToolDescriptor
	Execute(ctx context.Context, argsJSON string) (Result, error)
}

// Registerer is the small surface RegisterFunc needs from a tool
// container. *Registry implements it; adopter-side wrappers like
// builder.GlobalCatalog implement it too because they expose the same
// Register(Tool) shape. Using an interface here lets RegisterFunc
// target either without the caller bridging.
type Registerer interface {
	Register(t Tool)
}

// progressKey is the unexported context key for the progress reporting function.
type progressKey struct{}

// WithProgressFunc injects a progress-reporting callback into ctx. The agent
// loop calls this before invoking Execute so that long-running tools can emit
// "tool_progress" stream events without knowing about the streaming layer.
//
// The returned context should be passed directly to tool.Execute.
func WithProgressFunc(ctx context.Context, f func(msg string)) context.Context {
	return context.WithValue(ctx, progressKey{}, f)
}

// toolCallIDKey is the unexported context key for the per-call correlation ID.
type toolCallIDKey struct{}

// WithToolCallID injects a per-Execute correlation ID into ctx. The agent loop
// generates one ID per tool dispatch (separate from the LLM-issued tool-call
// ID, which is not always unique across providers — Gemini reuses tool names).
// Middleware that logs tool I/O reads the ID via ToolCallIDFromContext to pair
// entry and exit lines reliably even when SpeculativeTools=true interleaves
// parallel calls.
func WithToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, id)
}

// ToolCallIDFromContext returns the correlation ID set by WithToolCallID, or
// "" when called outside an agent loop.
func ToolCallIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(toolCallIDKey{}).(string); ok {
		return id
	}
	return ""
}

// ReportProgress emits a progress message if a reporting function was injected
// into ctx by WithProgressFunc. It is a no-op when called outside an agent loop
// (e.g. in tests that do not set up the reporting function).
//
// Call this inside Execute to emit status updates while a long-running operation
// is in progress:
//
//	tools.ReportProgress(ctx, "Polling operation (30s elapsed)…")
func ReportProgress(ctx context.Context, msg string) {
	if f, ok := ctx.Value(progressKey{}).(func(string)); ok && f != nil {
		f(msg)
	}
}

// Registry manages the available tools for an agent. Safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	tools   map[string]Tool
	wrapped map[string]Tool // cached debug-wrapped tools
	debug   bool
	logger  *slog.Logger
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// EnableDebug turns on per-call logging for every tool in this registry.
// Pass nil to use slog.Default(). Calls are idempotent — calling again swaps the logger.
//
//	reg.EnableDebug(nil)                          // use default logger
//	reg.EnableDebug(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
func (r *Registry) EnableDebug(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.debug = true
	r.logger = logger
	r.rebuildWrappedLocked()
}

// Register adds a tool to the registry. Overwrites any existing tool with the same name.
func (r *Registry) Register(t Tool) {
	name := t.Descriptor().Name
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[name] = t
	if r.debug {
		r.wrapToolLocked(name, t)
	} else {
		delete(r.wrapped, name)
	}
}

// Get retrieves a tool by name. Returns false if not found.
// When debug mode is enabled the tool is transparently wrapped with WithLogging.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok {
		return nil, false
	}
	if r.debug {
		t = r.debugWrapped(name, t)
	}
	return t, true
}

// Len returns the number of registered tools. Cheap branch for hot-path
// gates that only need to know "how many" — avoids the sort+slice+copy
// cost of All.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// All returns every registered tool in deterministic alphabetical order.
// Debug wrapping is applied when enabled.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]Tool, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		if r.debug {
			t = r.debugWrapped(name, t)
		}
		result = append(result, t)
	}
	return result
}

// Clone returns a shallow copy of the registry with the same tools, debug
// flag, and logger. Changes to the clone do not affect the original.
func (r *Registry) Clone() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := &Registry{
		tools:  make(map[string]Tool, len(r.tools)),
		debug:  r.debug,
		logger: r.logger,
	}
	maps.Copy(c.tools, r.tools)
	if c.debug {
		// c is not published yet, so this needs no lock of its own. Rebuild
		// rather than share the source map: the clone must be independently
		// mutable, and wrappers are cheap to reconstruct.
		c.rebuildWrappedLocked()
	}
	return c
}

// debugWrapped returns the pre-built logging wrapper for name, or t when
// none exists. This is a pure read, which is the point: it is reached from
// Get and All under a read lock, so it must never populate the cache.
//
// Building lazily here was a latent data race — an RLock is shared, not
// exclusive, so two concurrent Get calls in debug mode raced on the map and
// could trip a concurrent map write. The loop dispatches tools in parallel,
// and debug mode is exactly what someone turns on to investigate that.
// Wrappers are now built where the write lock is already held.
func (r *Registry) debugWrapped(name string, t Tool) Tool {
	if w, ok := r.wrapped[name]; ok {
		return w
	}
	return t
}

// wrapToolLocked builds and stores the debug wrapper for one tool.
// Caller must hold r.mu for writing.
func (r *Registry) wrapToolLocked(name string, t Tool) {
	if r.wrapped == nil {
		r.wrapped = make(map[string]Tool, len(r.tools))
	}
	r.wrapped[name] = Chain(t, WithLogging(r.logger))
}

// rebuildWrappedLocked regenerates every debug wrapper from scratch.
// Caller must hold r.mu for writing.
func (r *Registry) rebuildWrappedLocked() {
	r.wrapped = make(map[string]Tool, len(r.tools))
	for name, t := range r.tools {
		r.wrapToolLocked(name, t)
	}
}
