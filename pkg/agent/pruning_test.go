package agent

import (
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// ── Benchmarks ────────────────────────────────────────────────────────────────

func benchmarkMsgs(toolContentLen int) []history.Message {
	return []history.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "give me a long result"},
		{Role: "tool", Content: strings.Repeat("x", toolContentLen)},
		{Role: "user", Content: "follow-up"},
	}
}

func BenchmarkPruneContextMessages_Short(b *testing.B) {
	msgs := benchmarkMsgs(100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pruneContextMessages(msgs, 3)
	}
}

func BenchmarkPruneContextMessages_SoftTrim(b *testing.B) {
	msgs := benchmarkMsgs(softTrimThreshold + 5000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pruneContextMessages(msgs, 1)
	}
}

func BenchmarkPruneContextMessages_OutlierTrim(b *testing.B) {
	msgs := benchmarkMsgs(outlierTrimThreshold + 10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pruneContextMessages(msgs, 1)
	}
}

func TestPruneContextMessages_ShortMessagesUntouched(t *testing.T) {
	msgs := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
		{Role: "tool", Content: "short result"},
	}
	pruned := pruneContextMessages(msgs, 1)
	if pruned[2].Content != "short result" {
		t.Fatal("short tool message should not be pruned")
	}
}

func TestPruneContextMessages_SoftTrim(t *testing.T) {
	longContent := strings.Repeat("x", softTrimThreshold+1000)
	msgs := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "tool", Content: longContent},
		{Role: "user", Content: "latest"},
	}
	pruned := pruneContextMessages(msgs, 1)
	if len(pruned[1].Content) >= len(longContent) {
		t.Fatal("expected soft trimmed content to be shorter")
	}
	if !strings.Contains(pruned[1].Content, "chars truncated") {
		t.Fatal("expected truncation marker in trimmed content")
	}
}

func TestPruneContextMessages_OutlierGuard(t *testing.T) {
	hugeContent := strings.Repeat("x", outlierTrimThreshold+1)
	msgs := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "tool", Content: hugeContent},
		{Role: "user", Content: "latest"},
	}
	pruned := pruneContextMessages(msgs, 1)
	if !strings.Contains(pruned[1].Content, "Outlier Payload Truncated") {
		t.Fatal("expected outlier truncation")
	}
}

func TestPruneContextMessages_ProtectsRecentMessages(t *testing.T) {
	longContent := strings.Repeat("x", softTrimThreshold+1000)
	msgs := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "tool", Content: longContent}, // protected (within last 3)
		{Role: "user", Content: "latest"},
	}
	pruned := pruneContextMessages(msgs, 3) // protect all 3
	if pruned[1].Content != longContent {
		t.Fatal("protected message should not be pruned")
	}
}

func TestPruneContextMessages_SystemAndUserNeverPruned(t *testing.T) {
	longContent := strings.Repeat("x", softTrimThreshold+1000)
	msgs := []history.Message{
		{Role: "system", Content: longContent},
		{Role: "user", Content: longContent},
		{Role: "assistant", Content: "short"},
	}
	pruned := pruneContextMessages(msgs, 1)
	if pruned[0].Content != longContent {
		t.Fatal("system message should never be pruned")
	}
	if pruned[1].Content != longContent {
		t.Fatal("user message should never be pruned")
	}
}

// ── PatchDanglingToolCalls ──────────────────────────────────────────────────

func TestPatchDanglingToolCalls_Empty(t *testing.T) {
	got := patchDanglingToolCalls(nil)
	if len(got) != 0 {
		t.Fatalf("expected empty, got %d messages", len(got))
	}
}

func TestPatchDanglingToolCalls_NoToolCalls(t *testing.T) {
	msgs := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	got := patchDanglingToolCalls(msgs)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages unchanged, got %d", len(got))
	}
}

func TestPatchDanglingToolCalls_FullyMatched(t *testing.T) {
	msgs := []history.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "", ToolCalls: []history.ToolCall{{ID: "t1", Name: "echo"}}},
		{Role: "tool", Content: "echo:go", ToolCallID: "t1"},
		{Role: "assistant", Content: "done"},
	}
	got := patchDanglingToolCalls(msgs)
	if len(got) != 4 {
		t.Fatalf("expected 4 messages (no injection), got %d", len(got))
	}
}

func TestPatchDanglingToolCalls_PartialDangling(t *testing.T) {
	msgs := []history.Message{
		{Role: "assistant", Content: "", ToolCalls: []history.ToolCall{
			{ID: "t1", Name: "echo"},
			{ID: "t2", Name: "echo"},
		}},
		{Role: "tool", Content: "reply", ToolCallID: "t1"},
	}
	got := patchDanglingToolCalls(msgs)
	if len(got) != 3 {
		t.Fatalf("expected synthetic tool msg injected, got %d", len(got))
	}
	injected := got[2]
	if injected.Role != "tool" || injected.ToolCallID != "t2" || !injected.IsError {
		t.Fatalf("expected synthetic tool msg for t2 with IsError=true, got %+v", injected)
	}
	if !strings.Contains(injected.Content, "cancelled") {
		t.Fatalf("expected 'cancelled' in injected content, got %q", injected.Content)
	}
}

func TestPatchDanglingToolCalls_AllDangling(t *testing.T) {
	msgs := []history.Message{
		{Role: "assistant", ToolCalls: []history.ToolCall{
			{ID: "a", Name: "x"},
			{ID: "b", Name: "x"},
			{ID: "c", Name: "x"},
		}},
	}
	got := patchDanglingToolCalls(msgs)
	if len(got) != 4 {
		t.Fatalf("expected 1 original + 3 synthetic, got %d", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i+1].ToolCallID != want {
			t.Fatalf("synthetic msg #%d: expected tool_call_id %q, got %q", i, want, got[i+1].ToolCallID)
		}
	}
}

func TestPatchDanglingToolCallsExported(t *testing.T) {
	// PatchDanglingToolCalls is the public alias adopters call when they
	// slice history for ad-hoc LLM rounds (titling, summarisation, …).
	// Confirm it matches the internal helper's contract.
	msgs := []history.Message{
		{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "t1", Name: "x"}}},
	}
	got := PatchDanglingToolCalls(msgs)
	if len(got) != 2 {
		t.Fatalf("expected dangling tool_use sealed, got %d msgs", len(got))
	}
	if got[1].Role != "tool" || got[1].ToolCallID != "t1" {
		t.Fatalf("synthetic tool msg shape wrong: %+v", got[1])
	}
}

func TestPatchDanglingToolCalls_DanglingBeforeUserTurn(t *testing.T) {
	msgs := []history.Message{
		{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "t1", Name: "x"}}},
		{Role: "user", Content: "new turn"},
	}
	got := patchDanglingToolCalls(msgs)
	if len(got) != 3 {
		t.Fatalf("expected synthetic before user turn, got %d", len(got))
	}
	if got[1].Role != "tool" || got[1].ToolCallID != "t1" {
		t.Fatalf("synthetic must be inserted between assistant and user, got: %+v", got[1])
	}
	if got[2].Role != "user" {
		t.Fatalf("user turn should follow synthetic tool msg, got: %+v", got[2])
	}
}

// ── TruncateToolArguments ───────────────────────────────────────────────────

func TestTruncateToolArguments_ShortContent(t *testing.T) {
	msgs := []history.Message{{Role: "tool", Content: "short"}}
	got := truncateToolArguments(msgs)
	if got[0].Content != "short" {
		t.Fatalf("short content should not be truncated, got %q", got[0].Content)
	}
}

func TestTruncateToolArguments_LongContent(t *testing.T) {
	long := strings.Repeat("x", toolArgTruncateLen+500)
	msgs := []history.Message{{Role: "tool", Content: long}}
	got := truncateToolArguments(msgs)
	if len(got[0].Content) >= len(long) {
		t.Fatalf("expected truncation, got len %d >= %d", len(got[0].Content), len(long))
	}
	if !strings.Contains(got[0].Content, "truncated by system") {
		t.Fatalf("expected truncation marker, got %q", got[0].Content)
	}
}

func TestTruncateToolArguments_NonToolMessagesUntouched(t *testing.T) {
	long := strings.Repeat("y", toolArgTruncateLen+500)
	msgs := []history.Message{
		{Role: "user", Content: long},
		{Role: "assistant", Content: long},
		{Role: "system", Content: long},
	}
	got := truncateToolArguments(msgs)
	for i, m := range got {
		if m.Content != long {
			t.Fatalf("non-tool message #%d should not be truncated", i)
		}
	}
}

func TestTruncateToolArguments_UTF8Safe(t *testing.T) {
	// Each CJK char is 3 bytes but 1 rune. Make a content well over the limit.
	cjk := strings.Repeat("漢字", toolArgTruncateLen)
	msgs := []history.Message{{Role: "tool", Content: cjk}}
	got := truncateToolArguments(msgs)
	// Result must be valid UTF-8 (no corruption from byte-level cut).
	if !isValidUTF8(got[0].Content) {
		t.Fatalf("truncation corrupted UTF-8: %q", got[0].Content)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' && len(s) > 0 {
			// A U+FFFD in input could be legitimate; but we don't generate it,
			// so treat its presence as corruption from our cut.
			return false
		}
	}
	return true
}
