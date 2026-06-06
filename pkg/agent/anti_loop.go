package agent

import (
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// loopKillThreshold is the number of identical consecutive calls before the loop is killed.
const loopKillThreshold = 5

// loopWarnThreshold is the number of identical consecutive calls before a warning is injected.
const loopWarnThreshold = 3

// maxRecentCalls bounds the ring buffer holding the most recent tool calls
// inspected for loop detection. 30 is empirically enough to catch every
// pattern the detector cares about while keeping the struct cache-friendly.
const maxRecentCalls = 30

// loopWarnPrefix opens every anti-loop warning Detect emits. Both the warning
// format strings below and loopWarnMarker derive from it so the two can never
// drift apart — editing the wording can't silently break the strip.
const loopWarnPrefix = "[SYSTEM WARNING:"

// loopWarnMarker is the boundary at which runToolCall's appended warning begins
// in a persisted tool result (loop_execute.go appends "\n\n" + warning). The
// live AddCall path hashes the raw result (the warning is appended afterwards),
// but loopDetectorFromHistory re-reads the persisted content, which carries the
// warning. Because the warning embeds the consecutive count ("3 times" vs
// "4 times"), each persisted result would otherwise hash differently, so the
// kill threshold could never be reached across turns. Stripping at this marker
// restores byte-identity with the live path.
const loopWarnMarker = "\n\n" + loopWarnPrefix

// stripLoopWarning removes the anti-loop warning suffix appended to a persisted
// tool result so its hash matches the live raw result the model first saw.
func stripLoopWarning(content string) string {
	before, _, _ := strings.Cut(content, loopWarnMarker)
	return before
}

// callEntry records a single tool invocation for loop detection. Hashes are
// FNV-64 sums of args/result — equality is the only operation performed on
// them, so cryptographic strength buys nothing.
type callEntry struct {
	ToolName   string
	ArgsHash   uint64
	ResultHash uint64
}

func hashStr(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// loopDetector acts as an Agent Supervisor watching for infinite spirals.
// Safe for concurrent use. The ring buffer is fixed-size so AddCall never
// allocates; eviction is implicit (newest write overwrites oldest entry).
type loopDetector struct {
	mu    sync.Mutex
	ring  [maxRecentCalls]callEntry
	head  int // index of the next write
	count int // number of valid entries (capped at maxRecentCalls)
}

// newLoopDetector creates a fresh loop detector. Create one per agent run.
func newLoopDetector() *loopDetector {
	return &loopDetector{}
}

// loopDetectorFromHistory builds a loop detector pre-seeded with every paired
// assistant tool_call + tool_result found in msgs. Without this, a fresh
// detector resets on every iterateMessages entry (StartChat / Regenerate /
// Continue) and misses cross-turn loops — Phin observed Claude Sonnet 4.6
// calling mongo_sample on the same empty collection across four separate
// turns without ever tripping the per-turn detector.
//
// False positives are prevented by Detect()'s own break-on-different-name
// logic: even if the ring holds calls to many tools, only the contiguous
// trailing run of the same tool counts against the current call. Cap the
// seed work at maxRecentCalls — anything older would be overwritten by the
// ring anyway.
func loopDetectorFromHistory(msgs []history.Message) *loopDetector {
	ld := newLoopDetector()
	if len(msgs) == 0 {
		return ld
	}
	// Pre-index tool results by ID so paired lookup is O(1) per ToolCall.
	results := make(map[string]string, len(msgs)/2+1)
	for i := range msgs {
		if msgs[i].Role == "tool" && msgs[i].ToolCallID != "" {
			results[msgs[i].ToolCallID] = stripLoopWarning(msgs[i].Content)
		}
	}
	// Collect entries in chronological order. We cap at maxRecentCalls
	// before hashing — older calls would be evicted by the ring on AddCall,
	// so hashing them is wasted FNV work. Use a small dynamically-grown
	// slice and trim the head once it exceeds capacity.
	type pair struct{ name, args, result string }
	seeded := make([]pair, 0, maxRecentCalls)
	for i := range msgs {
		if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		for _, tc := range msgs[i].ToolCalls {
			r, ok := results[tc.ID]
			if !ok {
				continue
			}
			seeded = append(seeded, pair{name: tc.Name, args: tc.Arguments, result: r})
			if len(seeded) > maxRecentCalls {
				seeded = seeded[len(seeded)-maxRecentCalls:]
			}
		}
	}
	for _, p := range seeded {
		ld.AddCall(p.name, p.args, p.result)
	}
	return ld
}

// AddCall records a tool invocation with hashed args and result for pattern detection.
func (ld *loopDetector) AddCall(toolName, argsJSON, result string) {
	entry := callEntry{
		ToolName:   toolName,
		ArgsHash:   hashStr(argsJSON),
		ResultHash: hashStr(result),
	}
	ld.mu.Lock()
	ld.ring[ld.head] = entry
	ld.head = (ld.head + 1) % maxRecentCalls
	if ld.count < maxRecentCalls {
		ld.count++
	}
	ld.mu.Unlock()
}

// Len returns the number of entries currently held by the ring buffer.
// Useful for tests and observability; caps at maxRecentCalls.
func (ld *loopDetector) Len() int {
	ld.mu.Lock()
	defer ld.mu.Unlock()
	return ld.count
}

// at returns the entry k positions before the most recent write.
// Caller must hold ld.mu.
func (ld *loopDetector) at(k int) callEntry {
	idx := (ld.head - 1 - k + maxRecentCalls) % maxRecentCalls
	return ld.ring[idx]
}

// Detect returns a warning string to inject to the LLM, or an error if it should kill the run.
func (ld *loopDetector) Detect() (warning string, killErr error) {
	ld.mu.Lock()
	defer ld.mu.Unlock()

	n := ld.count
	if n < loopWarnThreshold {
		return "", nil
	}

	lastCall := ld.at(0)
	identicalCount := 0
	sameResultCount := 0

	for k := range n {
		prev := ld.at(k)
		if prev.ToolName != lastCall.ToolName {
			break
		}
		if prev.ArgsHash == lastCall.ArgsHash && prev.ResultHash == lastCall.ResultHash {
			identicalCount++
		}
		if prev.ArgsHash != lastCall.ArgsHash && prev.ResultHash == lastCall.ResultHash {
			sameResultCount++
		}
	}

	if identicalCount >= loopKillThreshold {
		return "", fmt.Errorf("agent stuck in identical loop calling %s with same args", lastCall.ToolName)
	} else if identicalCount >= loopWarnThreshold {
		return fmt.Sprintf(loopWarnPrefix+" You have called %s with the exact same arguments %d times consecutively. STOP doing this and try a different approach.]", lastCall.ToolName, identicalCount), nil
	}

	if sameResultCount >= loopKillThreshold {
		return "", fmt.Errorf("agent stuck in identical-result loop calling %s", lastCall.ToolName)
	} else if sameResultCount >= loopWarnThreshold {
		return fmt.Sprintf(loopWarnPrefix+" You have called %s with different arguments, but the outcome is identically unhelpful %d times in a row. Re-evaluate your overall strategy.]", lastCall.ToolName, sameResultCount), nil
	}

	return "", nil
}
