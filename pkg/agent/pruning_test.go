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
		PruneContextMessages(msgs, 3)
	}
}

func BenchmarkPruneContextMessages_SoftTrim(b *testing.B) {
	msgs := benchmarkMsgs(SoftTrimThreshold + 5000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		PruneContextMessages(msgs, 1)
	}
}

func BenchmarkPruneContextMessages_OutlierTrim(b *testing.B) {
	msgs := benchmarkMsgs(OutlierTrimThreshold + 10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		PruneContextMessages(msgs, 1)
	}
}

func TestPruneContextMessages_ShortMessagesUntouched(t *testing.T) {
	msgs := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
		{Role: "tool", Content: "short result"},
	}
	pruned := PruneContextMessages(msgs, 1)
	if pruned[2].Content != "short result" {
		t.Fatal("short tool message should not be pruned")
	}
}

func TestPruneContextMessages_SoftTrim(t *testing.T) {
	longContent := strings.Repeat("x", SoftTrimThreshold+1000)
	msgs := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "tool", Content: longContent},
		{Role: "user", Content: "latest"},
	}
	pruned := PruneContextMessages(msgs, 1)
	if len(pruned[1].Content) >= len(longContent) {
		t.Fatal("expected soft trimmed content to be shorter")
	}
	if !strings.Contains(pruned[1].Content, "omitted by GopherAgent Pruning") {
		t.Fatal("expected pruning marker in trimmed content")
	}
}

func TestPruneContextMessages_OutlierGuard(t *testing.T) {
	hugeContent := strings.Repeat("x", OutlierTrimThreshold+1)
	msgs := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "tool", Content: hugeContent},
		{Role: "user", Content: "latest"},
	}
	pruned := PruneContextMessages(msgs, 1)
	if !strings.Contains(pruned[1].Content, "Outlier Payload Truncated") {
		t.Fatal("expected outlier truncation")
	}
}

func TestPruneContextMessages_ProtectsRecentMessages(t *testing.T) {
	longContent := strings.Repeat("x", SoftTrimThreshold+1000)
	msgs := []history.Message{
		{Role: "system", Content: "sys"},
		{Role: "tool", Content: longContent}, // protected (within last 3)
		{Role: "user", Content: "latest"},
	}
	pruned := PruneContextMessages(msgs, 3) // protect all 3
	if pruned[1].Content != longContent {
		t.Fatal("protected message should not be pruned")
	}
}

func TestPruneContextMessages_SystemAndUserNeverPruned(t *testing.T) {
	longContent := strings.Repeat("x", SoftTrimThreshold+1000)
	msgs := []history.Message{
		{Role: "system", Content: longContent},
		{Role: "user", Content: longContent},
		{Role: "assistant", Content: "short"},
	}
	pruned := PruneContextMessages(msgs, 1)
	if pruned[0].Content != longContent {
		t.Fatal("system message should never be pruned")
	}
	if pruned[1].Content != longContent {
		t.Fatal("user message should never be pruned")
	}
}
