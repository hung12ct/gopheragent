package agent

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/memory"
)

// MemoryScopeFunc maps an (ctx, sessionKey) pair to the scope used for
// memory reads and writes. The default — derived inside the loop — returns
// sessionKey unchanged, which keeps memory isolated per-conversation.
//
// Override to share memory across sessions for the same user/tenant:
//
//	agent.WithMemoryScope(func(ctx context.Context, _ string) string {
//	    if u, ok := ctx.Value(userIDKey{}).(string); ok {
//	        return "user:" + u
//	    }
//	    return "anonymous"
//	})
type MemoryScopeFunc func(ctx context.Context, sessionKey string) string

// defaultMemoryScope returns sessionKey as the scope. Used when no
// MemoryScopeFn is configured.
func defaultMemoryScope(_ context.Context, sessionKey string) string {
	return sessionKey
}

// loadMemoryNotes returns the formatted note block for the configured
// scope. Returns "" when memory is disabled, the scope has no notes, or
// the store errors (errors are logged, not surfaced — memory is
// best-effort context, never load-bearing for correctness).
func (al *AgentLoop) loadMemoryNotes(ctx context.Context, sessionKey string) string {
	if al.Memory == nil {
		return ""
	}
	scopeFn := al.MemoryScopeFn
	if scopeFn == nil {
		scopeFn = defaultMemoryScope
	}
	scope := scopeFn(ctx, sessionKey)
	notes, err := al.Memory.List(ctx, scope)
	if err != nil {
		log.Printf("[gopheragent] memory list error for scope %q: %v", scope, err)
		return ""
	}
	return memory.FormatNotes(notes)
}

// memoryNotesKey is the ctx-value key for the formatted memory block. The
// loop stashes a single resolved string at runLogicLoop entry; every
// buildMsgsForLLM call within that Run reads it back via
// memoryNotesFromContext. Stashing on ctx (vs. a loop field) keeps
// concurrent sessions isolated without mutexes.
type memoryNotesKey struct{}

// withMemoryNotes returns ctx with notes attached. notes is the
// already-formatted block produced by memory.FormatNotes — the call site
// formats once per Run and ctx-propagates the string. Empty notes still
// install the key with "" so downstream reads stay deterministic.
func withMemoryNotes(ctx context.Context, notes string) context.Context {
	return context.WithValue(ctx, memoryNotesKey{}, notes)
}

// memoryNotesFromContext returns the formatted memory block stashed on
// ctx, or "" when none is present (memory disabled or empty scope).
func memoryNotesFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(memoryNotesKey{}).(string); ok {
		return v
	}
	return ""
}

// memoryNotesSentinel marks a system message that already has memory
// notes appended so retries within the same iteration don't double-inject.
const memoryNotesSentinel = "<!-- memory-notes -->"

// withMemoryNotesInSystem returns msgs with the formatted memory block
// appended to the first system message's Content. The sentinel makes
// repeat injection idempotent; the input slice is never mutated.
//
// Notes are appended after the base system prompt. Anthropic's prompt
// cache matches a contiguous prefix from byte 0, so any change to the
// notes block does invalidate the cached region — but notes only mutate
// between sessions (consolidator writes), never within one Run, so the
// in-Run cache stays warm and only the first call after consolidation
// pays a cache miss.
func (al *AgentLoop) withMemoryNotesInSystem(ctx context.Context, msgs []history.Message) []history.Message {
	notes := memoryNotesFromContext(ctx)
	if notes == "" {
		return msgs
	}
	if len(msgs) == 0 {
		return []history.Message{{Role: "system", Content: memoryNotesSentinel + strings.TrimLeft(notes, "\n")}}
	}
	// The sentinel only ever lands on msgs[0] (we only mutate the head
	// system message below). Checking just that index keeps the
	// idempotency guard O(1) instead of scanning the whole slice on
	// every iteration of a multi-turn session.
	if msgs[0].Role == "system" && strings.Contains(msgs[0].Content, memoryNotesSentinel) {
		return msgs
	}
	out := make([]history.Message, len(msgs))
	copy(out, msgs)
	if out[0].Role == "system" {
		out[0].Content = out[0].Content + memoryNotesSentinel + notes
		return out
	}
	// No system message at the head — synthesize one carrying the notes.
	return append([]history.Message{{Role: "system", Content: memoryNotesSentinel + strings.TrimLeft(notes, "\n")}}, out...)
}

// Consolidator distills a closed session's transcript into a small set of
// reusable Notes and writes them to a memory.Store.
//
// The default contract is one structured LLM call per Consolidate, so
// budget at most ~1k input tokens of transcript plus the configured
// MaxNotes worth of output. Skip Consolidate when the transcript is
// short — there's nothing to learn from a two-turn exchange.
//
// Concurrency: Consolidate is safe to call concurrently for distinct
// scopes; same-scope concurrency is allowed but may produce duplicate
// notes if two consolidations race on the same content. Callers that
// auto-consolidate after every Run should serialize per-scope.
type Consolidator struct {
	// Store is where extracted notes land. Required.
	Store memory.Store
	// LLM is the provider used to extract notes. Required.
	LLM LLMProvider
	// Prompt overrides the default extraction instruction. Empty
	// string falls back to defaultConsolidatorPrompt below — a neutral
	// "extract durable facts/preferences/mistakes" template that works
	// across domains.
	Prompt string
	// MaxNotes caps how many notes a single Consolidate emits. 0
	// applies the default of 8 — enough to capture a session's
	// distinct facts without bloating the next session's prompt.
	MaxNotes int
	// MinTranscriptMessages skips consolidation when the transcript
	// has fewer than this many non-system messages. 0 applies the
	// default of 3 — anything shorter rarely contains durable
	// knowledge worth the LLM round trip.
	MinTranscriptMessages int
}

const defaultConsolidatorPrompt = `You are a memory consolidator. Read the transcript below and extract durable knowledge that will help future sessions with the same user.

Emit at most {MAX_NOTES} notes. Each note must be:
- A single self-contained fact, preference, correction, or learned mistake.
- Phrased in present tense, third person ("user prefers X", "the foo table uses column bar").
- Concrete (no "agent should be careful").
- Useful in a future session even without this transcript.

Skip:
- Anything specific to this one session (e.g. "the user just asked about X").
- Anything the agent could re-derive cheaply from tools or context.
- Restatements of generic best practices.

Respond as JSON matching the schema.`

// consolidatorOutput is the JSON the LLM emits. Each ExtractedNote becomes
// one memory.Note via Consolidator.Consolidate.
type consolidatorOutput struct {
	Notes []extractedNote `json:"notes"`
}

type extractedNote struct {
	Key     string   `json:"key"`
	Content string   `json:"content"`
	Tags    []string `json:"tags,omitempty"`
}

// Consolidate reads transcript and writes any extracted notes to the
// store under scope. Returns the number of notes written and any error.
//
// Errors from the LLM call or store writes are returned. A nil transcript
// (or one below MinTranscriptMessages) is a no-op that returns (0, nil) —
// callers can blindly fire Consolidate after every turn without filtering.
func (c *Consolidator) Consolidate(ctx context.Context, scope string, transcript []history.Message) (int, error) {
	if c.Store == nil {
		return 0, fmt.Errorf("agent: consolidator: Store is nil")
	}
	if c.LLM == nil {
		return 0, fmt.Errorf("agent: consolidator: LLM is nil")
	}
	minMsgs := c.MinTranscriptMessages
	if minMsgs <= 0 {
		minMsgs = 3
	}
	if countNonSystem(transcript) < minMsgs {
		return 0, nil
	}
	maxNotes := c.MaxNotes
	if maxNotes <= 0 {
		maxNotes = 8
	}

	prompt := c.Prompt
	if prompt == "" {
		prompt = defaultConsolidatorPrompt
	}
	prompt = strings.ReplaceAll(prompt, "{MAX_NOTES}", strconv.Itoa(maxNotes))

	req := GenerateJSONRequest{
		Messages: []history.Message{
			{Role: "system", Content: prompt},
			{Role: "user", Content: "Transcript:\n" + renderTranscriptForConsolidation(transcript)},
		},
		Output: StructuredOutput{
			Name:        "consolidated_notes",
			Description: "Durable knowledge extracted from a session transcript.",
			Schema:      consolidatorSchema(),
			Strict:      true,
		},
	}

	var out consolidatorOutput
	if _, err := GenerateJSONInto(ctx, c.LLM, req, &out); err != nil {
		return 0, fmt.Errorf("agent: consolidator: %w", err)
	}

	written := 0
	for _, n := range out.Notes {
		if written >= maxNotes {
			break
		}
		if n.Key == "" || n.Content == "" {
			continue
		}
		note := memory.Note{Key: n.Key, Content: n.Content, Tags: n.Tags}
		if err := c.Store.Put(ctx, scope, note); err != nil {
			return written, fmt.Errorf("agent: consolidator: store put: %w", err)
		}
		written++
	}
	return written, nil
}

// consolidatorSchema returns the JSON-Schema enforced on the LLM output.
// Kept in a function so the map is fresh per call (callers that mutate
// the schema by accident don't corrupt later requests).
func consolidatorSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"notes": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"key":     map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
						"tags": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
						},
					},
					"required":             []string{"key", "content"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"notes"},
		"additionalProperties": false,
	}
}

// renderTranscriptForConsolidation flattens a message slice into a plain
// "role: content" log. Tool calls and tool results are included since
// they're often where mistakes/corrections live. System messages are
// dropped — the consolidator doesn't need to re-learn the agent's own
// instructions.
func renderTranscriptForConsolidation(msgs []history.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		if m.Content != "" {
			b.WriteString(m.Content)
		}
		for _, tc := range m.ToolCalls {
			b.WriteString("\n[tool_call ")
			b.WriteString(tc.Name)
			b.WriteString(" ")
			b.WriteString(tc.Arguments)
			b.WriteString("]")
		}
		if m.IsError {
			b.WriteString(" [error]")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func countNonSystem(msgs []history.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role != "system" {
			n++
		}
	}
	return n
}
