package agent

import (
	"crypto/sha256"
	"fmt"
	"sync"
)

// LoopKillThreshold is the number of identical consecutive calls before the loop is killed.
const LoopKillThreshold = 5

// LoopWarnThreshold is the number of identical consecutive calls before a warning is injected.
const LoopWarnThreshold = 3

// CallEntry records a single tool invocation for loop detection.
type CallEntry struct {
	ToolName   string
	ArgsHash   string
	ResultHash string
}

func hashStr(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

// LoopDetector acts as an Agent Supervisor watching for infinite spirals.
// Safe for concurrent use.
type LoopDetector struct {
	mu          sync.Mutex
	RecentCalls []CallEntry
}

// NewLoopDetector creates a fresh loop detector. Create one per agent run.
func NewLoopDetector() *LoopDetector {
	return &LoopDetector{
		RecentCalls: make([]CallEntry, 0, 30),
	}
}

// AddCall records a tool invocation with hashed args and result for pattern detection.
func (ld *LoopDetector) AddCall(toolName, argsJSON, result string) {
	ld.mu.Lock()
	defer ld.mu.Unlock()

	ld.RecentCalls = append(ld.RecentCalls, CallEntry{
		ToolName:   toolName,
		ArgsHash:   hashStr(argsJSON),
		ResultHash: hashStr(result),
	})
	if len(ld.RecentCalls) > 30 {
		ld.RecentCalls = ld.RecentCalls[1:]
	}
}

// Detect returns a warning string to inject to the LLM, or an error if it should kill the run.
func (ld *LoopDetector) Detect() (warning string, killErr error) {
	ld.mu.Lock()
	defer ld.mu.Unlock()

	n := len(ld.RecentCalls)
	if n < LoopWarnThreshold {
		return "", nil
	}

	lastCall := ld.RecentCalls[n-1]
	identicalCount := 0
	sameResultCount := 0

	for i := n - 1; i >= 0; i-- {
		prev := ld.RecentCalls[i]
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

	if identicalCount >= LoopKillThreshold {
		return "", fmt.Errorf("agent stuck in identical loop calling %s with same args", lastCall.ToolName)
	} else if identicalCount >= LoopWarnThreshold {
		return fmt.Sprintf("[SYSTEM WARNING: You have called %s with the exact same arguments %d times consecutively. STOP doing this and try a different approach.]", lastCall.ToolName, identicalCount), nil
	}

	if sameResultCount >= LoopKillThreshold {
		return "", fmt.Errorf("agent stuck in identical-result loop calling %s", lastCall.ToolName)
	} else if sameResultCount >= LoopWarnThreshold {
		return fmt.Sprintf("[SYSTEM WARNING: You have called %s with different arguments, but the outcome is identically unhelpful %d times in a row. Re-evaluate your overall strategy.]", lastCall.ToolName, sameResultCount), nil
	}

	return "", nil
}
