package agent

import (
	"fmt"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
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

func TestLoopDetectorFromHistory_SeedsAcrossTurns(t *testing.T) {
	// Reproduce Phin's pain: Claude calls mongo_sample with the same args
	// on the same empty collection across three prior turns. A fresh
	// detector resets each turn and never warns. The seeded detector
	// counts those prior calls so the 3rd identical call in the new turn
	// trips the loopWarnThreshold immediately.
	msgs := []history.Message{
		{Role: "user", Content: "look at the empty collection"},
		{Role: "assistant", ToolCalls: []history.ToolCall{
			{ID: "1", Name: "mongo_sample", Arguments: `{"coll":"x"}`},
		}},
		{Role: "tool", ToolCallID: "1", Content: "[]"},
		{Role: "assistant", Content: "Empty."},
		{Role: "user", Content: "look again"},
		{Role: "assistant", ToolCalls: []history.ToolCall{
			{ID: "2", Name: "mongo_sample", Arguments: `{"coll":"x"}`},
		}},
		{Role: "tool", ToolCallID: "2", Content: "[]"},
		{Role: "assistant", Content: "Still empty."},
	}
	ld := loopDetectorFromHistory(msgs)
	if ld.Len() != 2 {
		t.Fatalf("expected 2 seeded entries, got %d", ld.Len())
	}
	// Simulate a third identical call in the current turn — should warn.
	ld.AddCall("mongo_sample", `{"coll":"x"}`, "[]")
	warn, err := ld.Detect()
	if err != nil {
		t.Fatalf("unexpected kill: %v", err)
	}
	if warn == "" {
		t.Fatal("expected cross-turn warning after seeding two prior identical calls and adding one more")
	}
}

func TestLoopDetectorFromHistory_DanglingToolUseSkipped(t *testing.T) {
	// An assistant tool_use without a matching tool result (interrupted
	// turn, partial truncation) must not be seeded — there's no
	// ResultHash to count against future identical calls.
	msgs := []history.Message{
		{Role: "assistant", ToolCalls: []history.ToolCall{
			{ID: "orphan", Name: "mongo_sample", Arguments: `{"coll":"x"}`},
		}},
	}
	ld := loopDetectorFromHistory(msgs)
	if ld.Len() != 0 {
		t.Fatalf("dangling tool_use must not seed; got Len=%d", ld.Len())
	}
}

func TestLoopDetectorFromHistory_DifferentTrailingToolNoFalsePositive(t *testing.T) {
	// Many prior calls to tool_A; the current turn calls tool_B. Detect()
	// must not flag tool_B as a loop just because the ring is full of tool_A.
	msgs := []history.Message{}
	for i := 0; i < loopKillThreshold+1; i++ {
		id := fmt.Sprintf("a%d", i)
		msgs = append(msgs,
			history.Message{Role: "assistant", ToolCalls: []history.ToolCall{
				{ID: id, Name: "tool_a", Arguments: `{"x":1}`},
			}},
			history.Message{Role: "tool", ToolCallID: id, Content: "same"},
		)
	}
	ld := loopDetectorFromHistory(msgs)
	ld.AddCall("tool_b", `{"y":1}`, "result")
	warn, err := ld.Detect()
	if err != nil {
		t.Fatalf("unexpected kill on tool switch: %v", err)
	}
	if warn != "" {
		t.Fatalf("must not warn — tool switched; got: %s", warn)
	}
}

func TestLoopDetectorFromHistory_WarningSuffixDoesNotPoisonHash(t *testing.T) {
	// Regression (Phin memory_list loop): the anti-loop warning is appended to
	// the tool result before it is persisted (loop_execute.go). On the next turn
	// loopDetectorFromHistory re-reads that persisted content; because the warning
	// embeds the live consecutive count ("3 times" vs "4 times"), each result
	// would hash differently and loopKillThreshold could never be reached across
	// turns — the model loops forever, only ever re-warned. Stripping the warning
	// before hashing restores byte-identity with the live raw call so the kill
	// fires.
	const rawResult = `{"keys":[],"count":0}`
	var msgs []history.Message
	for i := 0; i < loopKillThreshold; i++ {
		id := fmt.Sprintf("m%d", i)
		content := rawResult
		// Mirror loop_execute.go: the warning is appended once the count crosses
		// the warn threshold, carrying the live count in its text.
		if n := i + 1; n >= loopWarnThreshold {
			content += fmt.Sprintf("\n\n[SYSTEM WARNING: You have called memory_list with the exact same arguments %d times consecutively. STOP doing this and try a different approach.]", n)
		}
		msgs = append(msgs,
			history.Message{Role: "assistant", ToolCalls: []history.ToolCall{
				{ID: id, Name: "memory_list", Arguments: `{"count":3}`},
			}},
			history.Message{Role: "tool", ToolCallID: id, Content: content},
		)
	}
	ld := loopDetectorFromHistory(msgs)
	if got := ld.Len(); got != loopKillThreshold {
		t.Fatalf("expected %d seeded entries, got %d", loopKillThreshold, got)
	}
	if _, err := ld.Detect(); err == nil {
		t.Fatal("expected kill after loopKillThreshold identical calls; warning suffix poisoned the result hash")
	}
}

func TestStripLoopWarning(t *testing.T) {
	const raw = `{"keys":[],"count":0}`
	if got := stripLoopWarning(raw); got != raw {
		t.Fatalf("raw result must pass through unchanged; got %q", got)
	}
	warned := raw + "\n\n[SYSTEM WARNING: You have called memory_list with the exact same arguments 4 times consecutively. STOP doing this and try a different approach.]"
	if got := stripLoopWarning(warned); got != raw {
		t.Fatalf("warning suffix must be stripped; got %q", got)
	}
}

func TestStripLoopWarning_MatchesRealDetectOutput(t *testing.T) {
	// Drift guard: the strip marker must remove a warning Detect actually
	// emits, not just a hardcoded literal. If the warning format is reworded
	// so it no longer begins with loopWarnPrefix, this fails — catching the
	// silent regression a literal-only test would miss.
	ld := newLoopDetector()
	for range loopWarnThreshold {
		ld.AddCall("tool1", `{"a":1}`, "same_result")
	}
	warn, err := ld.Detect()
	if err != nil {
		t.Fatalf("expected warn, got kill: %v", err)
	}
	if warn == "" {
		t.Fatal("expected a warning to guard against")
	}
	const raw = "raw result"
	persisted := raw + "\n\n" + warn // mirrors the append in loop_execute.go
	if got := stripLoopWarning(persisted); got != raw {
		t.Fatalf("real Detect warning not stripped — marker drifted from warning format; got %q", got)
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
