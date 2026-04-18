package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Tool-chaining reference syntax: the LLM can embed <output_of:ID.a.b> (or
// plain <output_of:ID>) inside the ArgsJSON of one tool call to reference the
// output of another tool call in the same turn. The scheduler parses these
// references, topologically orders the calls into execution waves, and
// substitutes each reference with the upstream result before the dependent
// call runs.
//
// The LLM must assign a stable ID to each tool call and use that same ID in
// the <output_of:...> token. Providers (OpenAI, Anthropic, Gemini) surface
// tool_call_id on their response schema, and all models reliably mirror the
// ID they emitted back into any reference token.
//
// Example from the LLM:
//
//	[
//	  {"id": "t1", "name": "fetch_user", "args": {"id": 1}},
//	  {"id": "t2", "name": "fetch_orders", "args": {"user_id": <output_of:t1.id>}}
//	]
//
// After t1 returns {"id": "abc123", "name": "Jane"}, t2 runs with
// {"user_id": "abc123"}.

// refPattern matches <output_of:ID> and <output_of:ID.a.b>. The ID is
// alphanumeric with underscores/hyphens; the path is dot-separated field names
// (same charset). The pattern also captures the leading/trailing JSON quote
// chars if the LLM wrapped the token in quotes — we strip those during
// substitution so scalar results produce valid JSON.
var refPattern = regexp.MustCompile(`"?<output_of:([A-Za-z0-9_\-]+)(?:\.([A-Za-z0-9_.\-]+))?>"?`)

// Ref is a parsed <output_of:...> reference.
type Ref struct {
	ID   string // tool-call ID being referenced
	Path string // optional dot-separated JSON path; "" means "full output"
}

// ParseRefs scans argsJSON for every <output_of:...> reference and returns
// them in appearance order. The same (ID,Path) pair is returned once per
// occurrence; callers that want uniqueness should dedupe.
func ParseRefs(argsJSON string) []Ref {
	matches := refPattern.FindAllStringSubmatch(argsJSON, -1)
	out := make([]Ref, 0, len(matches))
	for _, m := range matches {
		out = append(out, Ref{ID: m[1], Path: m[2]})
	}
	return out
}

// Resolver returns the raw result JSON (or plain string) for a completed
// tool-call ID. ok is false when the ID has not yet produced output.
type Resolver func(id string) (string, bool)

// Substitute replaces every <output_of:ID> / <output_of:ID.path> token in
// argsJSON with the resolved upstream value. Scalar results are JSON-encoded
// at the substitution site so the surrounding JSON remains well-formed; when
// the token is wrapped in quotes ("<output_of:...>") the quotes are consumed
// and the JSON-encoded value takes their place.
//
// Returns an error if any reference points to an unknown ID. A reference
// whose path does not exist in the output resolves to the JSON `null` literal
// — preserves best-effort chaining instead of aborting the wave.
func Substitute(argsJSON string, resolve Resolver) (string, error) {
	var firstErr error
	out := refPattern.ReplaceAllStringFunc(argsJSON, func(token string) string {
		m := refPattern.FindStringSubmatch(token)
		id, path := m[1], m[2]
		raw, ok := resolve(id)
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("agent: Substitute: unknown tool-call reference %q", id)
			}
			return token
		}
		val := extractPath(raw, path)
		encoded, err := json.Marshal(val)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("agent: Substitute: encode %q.%q: %w", id, path, err)
			}
			return token
		}
		return string(encoded)
	})
	return out, firstErr
}

// extractPath returns the JSON-decoded node at path inside raw. If raw is not
// valid JSON, the full string is returned for the empty path and nil for any
// non-empty path. Missing path segments resolve to nil.
func extractPath(raw, path string) any {
	if path == "" {
		var full any
		if err := json.Unmarshal([]byte(raw), &full); err == nil {
			return full
		}
		// Not JSON — treat as a plain string value.
		return raw
	}
	var node any
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		return nil
	}
	for seg := range strings.SplitSeq(path, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node, ok = m[seg]
		if !ok {
			return nil
		}
	}
	return node
}

// ScheduleToolCalls orders calls into execution waves respecting
// <output_of:...> dependencies: every call in wave i can run in parallel, and
// every call in wave i depends only on calls in waves 0..i-1.
//
// Calls with no references form wave 0. Calls that reference an unknown ID
// (not present in this batch) return an error — the caller should surface it
// to the LLM as a retry hint. Cyclic references likewise return an error.
//
// The input slice is not mutated; each wave preserves the caller's original
// tool-call ordering for deterministic behavior.
func ScheduleToolCalls(calls []PendingToolCall) ([][]PendingToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	byID := make(map[string]PendingToolCall, len(calls))
	order := make(map[string]int, len(calls))
	for i, c := range calls {
		if _, dup := byID[c.ID]; dup {
			return nil, fmt.Errorf("agent: ScheduleToolCalls: duplicate tool-call ID %q", c.ID)
		}
		byID[c.ID] = c
		order[c.ID] = i
	}

	// Build adjacency + indegree. deps[id] = set of IDs that id depends on.
	deps := make(map[string]map[string]struct{}, len(calls))
	indeg := make(map[string]int, len(calls))
	for _, c := range calls {
		indeg[c.ID] = 0
		deps[c.ID] = make(map[string]struct{})
	}
	for _, c := range calls {
		for _, ref := range ParseRefs(c.ArgsJSON) {
			if _, exists := byID[ref.ID]; !exists {
				return nil, fmt.Errorf("agent: ScheduleToolCalls: call %q references unknown ID %q", c.ID, ref.ID)
			}
			if ref.ID == c.ID {
				return nil, fmt.Errorf("agent: ScheduleToolCalls: call %q references itself", c.ID)
			}
			if _, seen := deps[c.ID][ref.ID]; seen {
				continue
			}
			deps[c.ID][ref.ID] = struct{}{}
			indeg[c.ID]++
		}
	}

	// Kahn's algorithm layered into waves.
	var waves [][]PendingToolCall
	remaining := len(calls)
	for remaining > 0 {
		var ready []string
		for id, n := range indeg {
			if n == 0 {
				ready = append(ready, id)
			}
		}
		if len(ready) == 0 {
			return nil, fmt.Errorf("agent: ScheduleToolCalls: cycle detected among tool calls")
		}
		sort.Slice(ready, func(i, j int) bool { return order[ready[i]] < order[ready[j]] })

		wave := make([]PendingToolCall, 0, len(ready))
		for _, id := range ready {
			wave = append(wave, byID[id])
			delete(indeg, id)
			remaining--
		}
		// Decrement indegrees of nodes depending on the ones we just scheduled.
		for depID, depSet := range deps {
			if _, stillPending := indeg[depID]; !stillPending {
				continue
			}
			for _, scheduled := range wave {
				if _, isDep := depSet[scheduled.ID]; isDep {
					indeg[depID]--
				}
			}
		}
		waves = append(waves, wave)
	}
	return waves, nil
}
