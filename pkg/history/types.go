// Package history provides message types and session storage backends for agent conversations.
package history

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// newForkKey returns a deterministic-format session key for a fork:
// "<parent>-fork-<8 hex chars>". The random suffix avoids collisions when
// the same session is forked multiple times in rapid succession.
func newForkKey(parent string) (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("history: generate fork key: %w", err)
	}
	return parent + "-fork-" + hex.EncodeToString(buf[:]), nil
}

// Message represents a single message in a chat conversation.
// It supports all standard roles: "system", "user", "assistant", and "tool".
//
// Multimodal input is expressed through Parts: when len(Parts) > 0, LLM
// provider adapters iterate Parts instead of (or in addition to) Content.
// When Parts is empty, adapters fall back to the plain-text Content path —
// so every existing caller and test continues to work unchanged.
//
// The fallback is only for an empty Parts. An adapter that cannot render a
// populated Parts must fail rather than quietly answer from Content: a
// caller that sent an image and got back a fluent, well-formed reply has no
// way to tell that the model never saw it, and nothing in the logs
// distinguishes that from success. Such an adapter should also report
// ImageInput=false through agent.CapabilityProvider so consumers that need
// vision can reject it at construction instead of mid-run.
type Message struct {
	Role       string      `json:"role"`
	Content    string      `json:"content"`
	Parts      []MediaPart `json:"parts,omitempty"`        // optional multimodal parts (images, richer text)
	ToolCallID string      `json:"tool_call_id,omitempty"` // for role:"tool" result messages
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`   // for role:"assistant" messages with tool calls
	IsError    bool        `json:"is_error,omitempty"`     // true when a tool execution failed

	// IsInlineResult is set on a role:"tool" message whose tool implements
	// tools.InlineRenderer with InlineResult()=true (e.g. generate_image,
	// generate_video). The agent loop already streamed the content inline
	// during the live turn, so render layers that filter out tool rows on
	// resume should special-case IsInlineResult — fold it back into the
	// preceding assistant message — rather than dropping it. Stable signal
	// across tool names; adopters no longer need a hand-curated allowlist.
	IsInlineResult bool `json:"is_inline_result,omitempty"`

	// CorrelationID is the opaque per-dispatch ID the agent loop generated
	// for this tool call (newToolCallID) and streamed live on the matching
	// ToolCallEvent.ID / ToolProgressEvent.ToolCallID. ToolCallID above holds
	// the provider's tool-call ID, which is load-bearing for tool_use /
	// tool_result adjacency but is NOT what live events expose (and which some
	// providers reuse across parallel same-name calls). CorrelationID gives
	// adopters a single stable handle to re-match a live-rendered artifact card
	// to its tool row after a session reload. Empty for pre-correlation
	// sessions and for non-tool messages.
	CorrelationID string `json:"correlation_id,omitempty"`

	// CacheHint requests that LLM adapters mark this message as a prompt-cache
	// breakpoint when the underlying provider supports it. The Anthropic
	// adapter translates CacheHint=true into `cache_control:{type:"ephemeral"}`
	// on the corresponding content block, which is cached for ~5 minutes and
	// cuts input-token cost ~90% on cached portions of subsequent requests.
	// Providers without a prompt-cache API (OpenAI/Gemini auto-cache server-side)
	// ignore this flag — it is always safe to set.
	CacheHint bool `json:"cache_hint,omitempty"`
}

// PartType enumerates the kinds of content a MediaPart can hold.
//
// Video and audio are intentionally omitted. Anthropic accepts neither in a
// message, and Gemini video requires Files-API upload semantics that do not
// fit the simple inline model. Video remains on the tool path via
// media_analyze; audio goes through pkg/audio, whose Transcriber converts it
// to text before it reaches a message.
//
// Transcribing rather than embedding is also the cheaper shape for anything
// long. History is re-sent on every LLM call in a session, so audio carried
// as a message part is re-uploaded on every subsequent turn — a one-hour
// recording would be billed again on each turn of the conversation about it.
//
// A part whose type an adapter cannot render fails the request with
// agent.ErrUnrenderablePart rather than being dropped.
type PartType string

const (
	// PartText is a plain-text fragment. Equivalent to the Content string when
	// used alone; useful when interleaved with image parts.
	PartText PartType = "text"
	// PartImage is an image. MIME must be set (e.g. "image/png", "image/jpeg").
	// Exactly one of URL or Data should be populated.
	PartImage PartType = "image"
)

// MediaPart is a typed chunk of a multimodal message.
//
// Usage rules:
//   - Type=="text": only Text is read.
//   - Type=="image": MIME is required; populate exactly one of URL or Data.
//     URL may be an https:// URL or a data: URI. Data is raw bytes and
//     will be base64-encoded by the adapter for providers that require it.
//
// The zero value is invalid — callers should use NewTextPart / NewImagePartURL
// / NewImagePartBytes for safety.
type MediaPart struct {
	Type PartType `json:"type"`
	Text string   `json:"text,omitempty"`
	MIME string   `json:"mime,omitempty"` // e.g. "image/png"; required for non-text parts
	URL  string   `json:"url,omitempty"`  // https://, http://, or data: URI
	Data []byte   `json:"data,omitempty"` // raw bytes; JSON-marshals as base64
}

// NewTextPart constructs a text-only MediaPart.
func NewTextPart(text string) MediaPart {
	return MediaPart{Type: PartText, Text: text}
}

// NewImagePartURL constructs an image part referencing a remote URL (https)
// or a data: URI. mime is a best-effort hint; providers that derive MIME
// from the URL ignore it.
func NewImagePartURL(mime, url string) MediaPart {
	return MediaPart{Type: PartImage, MIME: mime, URL: url}
}

// NewImagePartBytes constructs an image part from raw bytes. The adapter
// layer base64-encodes when the provider expects base64 and synthesizes a
// data: URI when the provider expects a URL.
func NewImagePartBytes(mime string, data []byte) MediaPart {
	return MediaPart{Type: PartImage, MIME: mime, Data: data}
}

// ToolCall represents a tool invocation declared by the assistant.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string
}

// Session represents a chat session with its full message history.
type Session struct {
	Key        string               `json:"key"`
	Messages   []Message            `json:"messages"`
	AsyncTasks map[string]AsyncTask `json:"async_tasks,omitempty"`
}

// AsyncTask represents a background task managed by the session.
type AsyncTask struct {
	TaskID    string `json:"task_id"`
	AgentName string `json:"agent_name"`
	Status    string `json:"status"` // "running", "success", "error", "cancelled", "interrupted"
	Result    string `json:"result,omitempty"`
}
