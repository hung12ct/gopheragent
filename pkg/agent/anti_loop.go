package agent

import (
	"fmt"
	"hash/fnv"
	"sync"
)

// loopKillThreshold is the number of identical consecutive calls before the loop is killed.
const loopKillThreshold = 5

// loopWarnThreshold is the number of identical consecutive calls before a warning is injected.
const loopWarnThreshold = 3

// maxRecentCalls bounds the ring buffer holding the most recent tool calls
// inspected for loop detection. 30 is empirically enough to catch every
// pattern the detector cares about while keeping the struct cache-friendly.
const maxRecentCalls = 30

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
		return fmt.Sprintf("[SYSTEM WARNING: You have called %s with the exact same arguments %d times consecutively. STOP doing this and try a different approach.]", lastCall.ToolName, identicalCount), nil
	}

	if sameResultCount >= loopKillThreshold {
		return "", fmt.Errorf("agent stuck in identical-result loop calling %s", lastCall.ToolName)
	} else if sameResultCount >= loopWarnThreshold {
		return fmt.Sprintf("[SYSTEM WARNING: You have called %s with different arguments, but the outcome is identically unhelpful %d times in a row. Re-evaluate your overall strategy.]", lastCall.ToolName, sameResultCount), nil
	}

	return "", nil
}
