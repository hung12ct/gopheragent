package agent

import (
	"fmt"
	"testing"
)

func TestLoopDetector_NoFalsePositiveBelowThreshold(t *testing.T) {
	ld := newLoopDetector()
	ld.AddCall("tool1", `{"a":1}`, "result1")
	ld.AddCall("tool1", `{"a":1}`, "result1")

	warn, err := ld.Detect()
	if err != nil {
		t.Fatalf("unexpected kill: %v", err)
	}
	if warn != "" {
		t.Fatalf("unexpected warning below threshold: %s", warn)
	}
}

func TestLoopDetector_WarnOnIdenticalLoop(t *testing.T) {
	ld := newLoopDetector()
	for i := 0; i < loopWarnThreshold; i++ {
		ld.AddCall("tool1", `{"a":1}`, "same_result")
	}

	warn, err := ld.Detect()
	if err != nil {
		t.Fatalf("should warn, not kill: %v", err)
	}
	if warn == "" {
		t.Fatal("expected warning for identical loop")
	}
}

func TestLoopDetector_KillOnIdenticalLoop(t *testing.T) {
	ld := newLoopDetector()
	for i := 0; i < loopKillThreshold; i++ {
		ld.AddCall("tool1", `{"a":1}`, "same_result")
	}

	_, err := ld.Detect()
	if err == nil {
		t.Fatal("expected kill error for identical loop")
	}
}

func TestLoopDetector_WarnOnSameResultDifferentArgs(t *testing.T) {
	ld := newLoopDetector()
	// Need loopWarnThreshold+1 calls: the last call's self-match counts as "identical" not "sameResult",
	// so sameResultCount is (total - 1). We need sameResultCount >= loopWarnThreshold.
	for i := 0; i < loopWarnThreshold+1; i++ {
		ld.AddCall("tool1", fmt.Sprintf(`{"attempt":%d,"padding":"unique-%d"}`, i, i*1000), "brick_wall")
	}

	warn, err := ld.Detect()
	if err != nil {
		t.Fatalf("should warn, not kill: %v", err)
	}
	if warn == "" {
		t.Fatal("expected warning for same-result loop")
	}
}

func TestLoopDetector_BreakOnDifferentTool(t *testing.T) {
	ld := newLoopDetector()
	ld.AddCall("tool_a", `{"a":1}`, "r1")
	ld.AddCall("tool_a", `{"a":1}`, "r1")
	ld.AddCall("tool_a", `{"a":1}`, "r1")
	ld.AddCall("tool_b", `{"b":1}`, "r2") // different tool breaks streak

	warn, err := ld.Detect()
	if err != nil {
		t.Fatalf("unexpected kill: %v", err)
	}
	if warn != "" {
		t.Fatalf("unexpected warning after tool change: %s", warn)
	}
}

func TestLoopDetector_MaxCapacity(t *testing.T) {
	ld := newLoopDetector()
	for i := 0; i < 50; i++ {
		ld.AddCall("tool1", `{}`, "r")
	}
	if got := ld.Len(); got > 30 {
		t.Fatalf("expected max 30 entries, got %d", got)
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

func BenchmarkLoopDetector_AddAndDetect(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ld := newLoopDetector()
		for j := 0; j < 10; j++ {
			ld.AddCall("tool1", `{"q":"search"}`, "some result text")
		}
		ld.Detect()
	}
}

func BenchmarkLoopDetector_DetectNoLoop(b *testing.B) {
	ld := newLoopDetector()
	for i := 0; i < 10; i++ {
		ld.AddCall(fmt.Sprintf("tool%d", i), `{"q":"search"}`, fmt.Sprintf("result%d", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ld.Detect()
	}
}
