package agent

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/memory"
)

// MemoryConfig tunes the loader's per-Run cost. The defaults applied by
// loadMemoryNotes when fields are zero are documented per-field below.
//
// Both bounds are enforced: the loader pulls at most MaxNotes from the
// store, then trims further to fit TokenBudget. Either alone may be
// sufficient — TokenBudget is the harder ceiling because it tracks the
// prompt impact directly; MaxNotes is a cheaper short-circuit that
// avoids loading huge result sets just to throw most of them away.
type MemoryConfig struct {
	// TokenBudget caps the prompt-token cost of the injected memory
	// block. 0 applies the default of 500 — enough for ~15 notes at
	// typical content length without dominating most system prompts.
	// Negative values are treated as 0. Estimation uses a 4-chars/
	// token heuristic; accurate within ±20% for English.
	TokenBudget int
	// MaxNotes caps the per-Run note count passed to FormatNotes. 0
	// applies the default of 50 — the typical upper bound a deployment
	// wants before TokenBudget would start trimming anyway. Negative
	// values are treated as 0.
	MaxNotes int
}

// memoryConfigOrDefault returns cfg with zero-valued fields replaced by
// the documented defaults. Centralizes the default policy so call sites
// don't drift over time.
func memoryConfigOrDefault(cfg MemoryConfig) MemoryConfig {
	if cfg.TokenBudget <= 0 {
		cfg.TokenBudget = 500
	}
	if cfg.MaxNotes <= 0 {
		cfg.MaxNotes = 50
	}
	return cfg
}

// MemoryScopeFunc maps an (ctx, sessionKey) pair to the scope used for
// memory reads and writes. The default — derived inside the loop —
// returns sessionKey unchanged, which keeps memory isolated
// per-conversation.
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

// memoryCharsPerToken is the heuristic used by the loader to convert
// the TokenBudget into a character budget. Matches estimateTokens'
// 4-chars/token rule elsewhere in the package.
const memoryCharsPerToken = 4

// resolveMemoryScope runs the configured MemoryScopeFn (or the
// default) and returns the resolved scope. Empty result signals the
// fail-closed path: typically an unauthenticated request whose
// resolver explicitly returned "" so memory must not be touched on
// this Run. Both the loader and the consolidator honor the empty
// signal by skipping their work.
func (al *AgentLoop) resolveMemoryScope(ctx context.Context, sessionKey string) string {
	scopeFn := al.MemoryScopeFn
	if scopeFn == nil {
		scopeFn = defaultMemoryScope
	}
	return scopeFn(ctx, sessionKey)
}

// loadMemoryForRun is the loader driver: resolves the scope, fetches
// notes bounded by MemoryConfig, trims to TokenBudget, and returns
// (scope, formatted block, note count) so the caller can both stash
// the block on ctx and emit a MemoryLoadedEvent with accurate metrics.
//
// scope == "" means memory is fully disabled for this Run (Memory
// nil, or the resolver returned "" — fail-closed). The caller skips
// the audit event in that case; "memory not used" is not an event
// worth logging.
//
// scope != "" but block == "" means the store errored or the scope
// is empty; the audit event still fires so adopters can record the
// attempt with NoteCount=0.
func (al *AgentLoop) loadMemoryForRun(ctx context.Context, sessionKey string) (scope, block string, count int) {
	if al.Memory == nil {
		return "", "", 0
	}
	scope = al.resolveMemoryScope(ctx, sessionKey)
	if scope == "" {
		return "", "", 0
	}
	cfg := memoryConfigOrDefault(al.MemoryCfg)
	notes, err := al.Memory.List(ctx, scope, memory.ListOpts{Limit: cfg.MaxNotes})
	if err != nil {
		log.Printf("[gopheragent] memory list error for scope %q: %v", scope, err)
		return scope, "", 0
	}
	notes = trimToTokenBudget(notes, cfg.TokenBudget)
	return scope, memory.FormatNotes(notes), len(notes)
}

// loadMemoryNotes returns just the formatted block. Kept as a thin
// wrapper so tests targeting the loader contract don't need to track
// the scope/count return shape. Internal callers prefer
// loadMemoryForRun.
func (al *AgentLoop) loadMemoryNotes(ctx context.Context, sessionKey string) string {
	_, block, _ := al.loadMemoryForRun(ctx, sessionKey)
	return block
}

// trimToTokenBudget returns the longest prefix of notes whose formatted
// rendering fits inside maxTokens. Pure function so the loader stays
// straightforward and the bound is testable in isolation.
//
// The estimate uses memoryCharsPerToken and the same per-bullet
// overhead FormatNotes produces (a leading "- " plus a newline). The
// header overhead (the "## Long-term memory" prelude) is included up
// front — if even the header doesn't fit, the loader returns no notes,
// which FormatNotes then renders as "".
func trimToTokenBudget(notes []memory.Note, maxTokens int) []memory.Note {
	if len(notes) == 0 || maxTokens <= 0 {
		return notes
	}
	budgetChars := maxTokens * memoryCharsPerToken
	// Subtract the FormatNotes header overhead before iterating.
	const headerChars = len("\n\n## Long-term memory\nFacts learned from prior sessions with this user. Use them to skip clarifying questions and avoid repeating past mistakes.\n")
	if budgetChars <= headerChars {
		return nil
	}
	budgetChars -= headerChars
	used := 0
	for i, n := range notes {
		if n.Content == "" {
			continue
		}
		// "- " + content + "\n"
		cost := 3 + len(n.Content)
		if used+cost > budgetChars {
			return notes[:i]
		}
		used += cost
	}
	return notes
}

// memoryNotesKey is the ctx-value key for the formatted memory block.
// The loop stashes a single resolved string at runLogicLoop entry; every
// buildMsgsForLLM call within that Run reads it back via
// memoryNotesFromContext. Stashing on ctx (vs. a loop field) keeps
// concurrent sessions isolated without mutexes.
type memoryNotesKey struct{}

// withMemoryNotes returns ctx with notes attached. notes is the
// already-formatted block produced by memory.FormatNotes — the call
// site formats once per Run and ctx-propagates the string. Empty notes
// still install the key with "" so downstream reads stay deterministic.
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

// Consolidator distills a closed session's transcript into Notes,
// merging with the scope's existing notes so dedupes, refinements, and
// stale-knowledge pruning happen in one LLM call. The output is the
// curated full state of the scope; the Store is updated atomically via
// ReplaceAll.
//
// The merge design is what bounds long-term growth: every Consolidate
// call sees the prior notes and decides whether to keep, merge, or
// drop each one. A model that picks variable keys can't accumulate
// forever because the merger collapses semantic duplicates and caps
// the output count at MaxNotes.
//
// Concurrency: Consolidate is safe to call concurrently for distinct
// scopes; same-scope concurrency is allowed but may produce
// last-writer-wins ReplaceAll behavior. Callers that auto-consolidate
// after every Run should serialize per-scope (the AgentLoop's
// fireConsolidator path does this implicitly by running once per Run
// in a detached goroutine).
type Consolidator struct {
	// Store is where extracted notes land. Required.
	Store memory.Store
	// LLM is the provider used to extract and merge notes. Required.
	LLM LLMProvider
	// Prompt overrides the default merge-and-extract instruction.
	// Empty string falls back to defaultConsolidatorPrompt below — a
	// neutral template that works across domains.
	Prompt string
	// MaxNotes caps the curated note count after merge. 0 applies the
	// default of 30 — small enough to stay under typical loader
	// TokenBudget after FormatNotes overhead, large enough to retain
	// the long tail of useful per-user knowledge.
	MaxNotes int
	// MinTranscriptMessages skips consolidation when the transcript
	// has fewer than this many non-system messages. 0 applies the
	// default of 3 — anything shorter rarely contains durable
	// knowledge worth the LLM round trip.
	MinTranscriptMessages int
	// FirePolicy controls how often the AgentLoop's post-Run hook
	// invokes Consolidate. The zero value applies DefaultFirePolicy
	// (MinInterval = 10m, no turn-count throttle) — a sane "don't
	// burn money" default for chat workloads where bursts of
	// related turns shouldn't each trigger a fresh LLM call. Set
	// Disabled = true to skip auto-fire entirely and drive
	// Consolidate manually from a cron / logout hook. See FirePolicy
	// for the per-field semantics.
	//
	// Direct calls to Consolidate (outside the AgentLoop hook) bypass
	// the policy — adopters running scheduled batch consolidations
	// retain full control of timing.
	FirePolicy FirePolicy

	// fireMu guards fireState. Held briefly during the shouldFire
	// check + bookkeeping; never held across the LLM call.
	fireMu    sync.Mutex
	fireState map[string]*scopeFireState
}

// FirePolicy throttles the AgentLoop's auto-fire path so a long
// session doesn't bill one LLM call per turn for consolidation.
//
// All three fields are independent; they're evaluated in order with
// short-circuit semantics:
//   - Disabled  : if true, fire is suppressed unconditionally.
//   - MinInterval : if non-zero, suppresses fires within wall-clock
//     interval of the prior fire for the same scope.
//   - NTurns    : if non-zero, suppresses fires until N completed Runs
//     have accumulated since the prior fire for the same scope.
//
// Combining MinInterval and NTurns AND's the conditions — both must
// allow before a fire happens. The first Run for a scope (no prior
// fire) is always allowed regardless of policy unless Disabled.
//
// Manual Consolidate calls bypass FirePolicy. Adopters running a
// nightly batch can set Disabled = true and call Consolidate
// directly from their scheduler.
type FirePolicy struct {
	// Disabled, when true, the AgentLoop's post-Run hook never
	// auto-fires. Adopters call Consolidate themselves.
	Disabled bool
	// NTurns suppresses auto-fires until this many completed Runs
	// have happened for the same scope since the last fire. 0
	// disables this throttle. Useful when conversations are slow
	// (one turn per hour) and the time-based throttle alone would
	// fire too often relative to information density.
	NTurns int
	// MinInterval enforces a minimum wall-clock gap between fires
	// for the same scope. 0 disables this throttle.
	MinInterval time.Duration
}

// DefaultFirePolicy is applied when Consolidator.FirePolicy is the
// zero value. The 10-minute interval matches the typical span of a
// conversation "burst" and amortizes the LLM cost roughly 10–15× for
// chat-heavy workloads vs. firing every Run. The first Run for a
// scope still fires (initial lastFiredAt is zero).
var DefaultFirePolicy = FirePolicy{MinInterval: 10 * time.Minute}

// scopeFireState tracks per-scope throttle bookkeeping. Mutated only
// while Consolidator.fireMu is held.
type scopeFireState struct {
	turnsSinceFire int
	lastFiredAt    time.Time // zero = never fired
}

// shouldFire reports whether the current Run is allowed to launch a
// consolidation under the configured FirePolicy. Returns true at most
// once per (MinInterval + NTurns) window per scope, and stamps
// lastFiredAt to "now" eagerly so a concurrent second Run launched
// before the consolidation goroutine finishes doesn't race-fire.
//
// Disabled policy always returns false — callers should not invoke
// shouldFire when Disabled, but the inner check is defensive.
func (c *Consolidator) shouldFire(scope string) bool {
	policy := c.FirePolicy
	if (policy == FirePolicy{}) {
		policy = DefaultFirePolicy
	}
	if policy.Disabled {
		return false
	}

	c.fireMu.Lock()
	defer c.fireMu.Unlock()
	if c.fireState == nil {
		c.fireState = make(map[string]*scopeFireState)
	}
	st, ok := c.fireState[scope]
	if !ok {
		st = &scopeFireState{}
		c.fireState[scope] = st
	}
	st.turnsSinceFire++

	// First fire for this scope: always allowed.
	if st.lastFiredAt.IsZero() {
		st.lastFiredAt = time.Now()
		st.turnsSinceFire = 0
		return true
	}
	// Both throttles must allow.
	if policy.MinInterval > 0 && time.Since(st.lastFiredAt) < policy.MinInterval {
		return false
	}
	if policy.NTurns > 0 && st.turnsSinceFire < policy.NTurns {
		return false
	}
	st.lastFiredAt = time.Now()
	st.turnsSinceFire = 0
	return true
}

// ConsolidateResult reports what changed in a single Consolidate call.
// Useful for telemetry/logging without re-reading the store. The
// "before" count is the size of the existing scope, "after" is the
// merged result.
type ConsolidateResult struct {
	Before int
	After  int
}

const defaultConsolidatorPrompt = `You are a memory consolidator for an AI agent. You receive two inputs:

1. EXISTING notes from prior sessions with this user — they may be stale, duplicated, slightly worded differently, or contradicted by what happened in the new transcript.
2. NEW TRANSCRIPT from the latest session.

Your job is to produce the CURATED FULL SET of at most {MAX_NOTES} notes that should persist as memory for future sessions. The output replaces the existing notes entirely — anything you omit is forgotten.

Curation rules:
- DROP notes the transcript contradicts (e.g. "user prefers metric" but they explicitly asked for imperial in this transcript).
- MERGE notes that overlap (same fact, different wording → one note with the clearest phrasing).
- ADD new durable facts, preferences, corrections, or learned mistakes the transcript reveals.
- DROP notes that are session-specific or unlikely to apply again.
- Prefer fewer, denser, higher-signal notes over many overlapping ones.
- Use STABLE, descriptive keys ("jira.default_workspace", not "pref_2026_05_20"). Prefer to overwrite an existing key by reusing it rather than inventing a new one for the same fact.

Each note must be:
- A single self-contained fact, preference, correction, or learned mistake.
- Phrased in present tense, third person ("user prefers X", "the foo table uses column bar").
- Concrete (no "agent should be careful").
- Useful in a future session even without this transcript.

Respond as JSON matching the schema.`

// consolidatorOutput is the JSON shape the LLM emits.
type consolidatorOutput struct {
	Notes []extractedNote `json:"notes"`
}

type extractedNote struct {
	Key     string   `json:"key"`
	Content string   `json:"content"`
	Tags    []string `json:"tags,omitempty"`
}

// Consolidate merges the scope's existing notes with knowledge extracted
// from transcript and atomically replaces the stored set. Returns the
// before/after counts and any error.
//
// A nil/short transcript (below MinTranscriptMessages) is a no-op that
// returns the current size with no error — callers can fire
// Consolidate unconditionally after every Run without filtering.
func (c *Consolidator) Consolidate(ctx context.Context, scope string, transcript []history.Message) (ConsolidateResult, error) {
	if c.Store == nil {
		return ConsolidateResult{}, fmt.Errorf("agent: consolidator: Store is nil")
	}
	if c.LLM == nil {
		return ConsolidateResult{}, fmt.Errorf("agent: consolidator: LLM is nil")
	}
	minMsgs := c.MinTranscriptMessages
	if minMsgs <= 0 {
		// Default 6 (≥3 round-trips). Single-question chats — user
		// asks, agent answers, optional one follow-up — almost never
		// produce durable cross-session knowledge worth the LLM cost.
		// Adopters with denser per-turn content set this to 3 or lower.
		minMsgs = 6
	}
	if countNonSystem(transcript) < minMsgs {
		existing, _ := c.Store.List(ctx, scope, memory.ListOpts{})
		return ConsolidateResult{Before: len(existing), After: len(existing)}, nil
	}
	maxNotes := c.MaxNotes
	if maxNotes <= 0 {
		maxNotes = 30
	}

	existing, err := c.Store.List(ctx, scope, memory.ListOpts{})
	if err != nil {
		return ConsolidateResult{}, fmt.Errorf("agent: consolidator: read existing: %w", err)
	}

	prompt := c.Prompt
	if prompt == "" {
		prompt = defaultConsolidatorPrompt
	}
	prompt = strings.ReplaceAll(prompt, "{MAX_NOTES}", strconv.Itoa(maxNotes))

	userPayload := buildConsolidatorUserPayload(existing, transcript)

	req := GenerateJSONRequest{
		Messages: []history.Message{
			{Role: "system", Content: prompt},
			{Role: "user", Content: userPayload},
		},
		Output: StructuredOutput{
			Name:        "consolidated_notes",
			Description: "Curated full set of memory notes for the scope.",
			Schema:      consolidatorSchema(),
			Strict:      true,
		},
	}

	var out consolidatorOutput
	if _, err := GenerateJSONInto(ctx, c.LLM, req, &out); err != nil {
		return ConsolidateResult{Before: len(existing)}, fmt.Errorf("agent: consolidator: %w", err)
	}

	curated := make([]memory.Note, 0, len(out.Notes))
	seen := make(map[string]struct{}, len(out.Notes))
	for _, n := range out.Notes {
		if len(curated) >= maxNotes {
			break
		}
		if n.Key == "" || n.Content == "" {
			continue
		}
		// De-dupe by key in the LLM output itself — a single response
		// returning the same key twice would otherwise produce a
		// ReplaceAll error or a silently-collapsed entry.
		if _, dup := seen[n.Key]; dup {
			continue
		}
		seen[n.Key] = struct{}{}
		curated = append(curated, memory.Note{Key: n.Key, Content: n.Content, Tags: n.Tags})
	}
	if err := c.Store.ReplaceAll(ctx, scope, curated); err != nil {
		return ConsolidateResult{Before: len(existing)}, fmt.Errorf("agent: consolidator: replace: %w", err)
	}
	return ConsolidateResult{Before: len(existing), After: len(curated)}, nil
}

// buildConsolidatorUserPayload renders the existing-notes + transcript
// block that goes into the user message. Kept separate so the format
// is easy to evolve without touching Consolidate's control flow.
func buildConsolidatorUserPayload(existing []memory.Note, transcript []history.Message) string {
	var b strings.Builder
	b.WriteString("EXISTING NOTES:\n")
	if len(existing) == 0 {
		b.WriteString("(none — this is the first consolidation for this scope)\n")
	} else {
		for _, n := range existing {
			b.WriteString("- key=")
			b.WriteString(n.Key)
			b.WriteString(": ")
			b.WriteString(n.Content)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nNEW TRANSCRIPT:\n")
	b.WriteString(renderTranscriptForConsolidation(transcript))
	return b.String()
}

// consolidatorSchema returns the JSON-Schema enforced on the LLM
// output. Fresh map per call so accidental mutation by a caller doesn't
// poison subsequent requests.
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
					"required":             []string{"key", "content", "tags"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"notes"},
		"additionalProperties": false,
	}
}

// renderTranscriptForConsolidation flattens a message slice into a
// plain "role: content" log. Tool calls and tool results are included
// since they're often where mistakes/corrections live. System messages
// are dropped — the consolidator doesn't need to re-learn the agent's
// own instructions.
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
