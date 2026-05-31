package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

func fiveRows() []map[string]any {
	rows := make([]map[string]any, 5)
	for i := range rows {
		rows[i] = map[string]any{"id": i}
	}
	return rows
}

// TestEmit_LLMPreviewRows_CapsTextNotHookOrStructured asserts WithLLMPreviewRows
// truncates ONLY the LLM-visible result text; the OnSQL hook (host grid) and
// the tools.Result.Structured payload still carry the full row set.
func TestEmit_LLMPreviewRows_CapsTextNotHookOrStructured(t *testing.T) {
	var hookEv SQLQueryEvent
	exec := &executeSQLTool{
		sessionKey:     "s",
		onSQL:          func(_ context.Context, ev SQLQueryEvent) { hookEv = ev },
		llmPreviewRows: 2,
	}
	res := SQLResult{SQL: "SELECT id FROM t", Columns: []string{"id"}, Rows: fiveRows(), RowCount: 5}

	out, err := exec.makeEmitFunc(context.Background())(res)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	// Host grid (hook) still sees all 5 rows.
	if len(hookEv.Rows) != 5 {
		t.Fatalf("hook rows: got %d, want 5 (full set)", len(hookEv.Rows))
	}
	// Structured (host via OnToolResult) carries the full set.
	if sr, ok := out.Structured.(SQLResult); !ok || len(sr.Rows) != 5 {
		t.Fatalf("Structured should carry full 5 rows, got %#v", out.Structured)
	}
	// LLM-visible Text carries only the preview, with the true RowCount and a
	// truncation signal so the model knows it saw a sample.
	var seen SQLResult
	if err := json.Unmarshal([]byte(out.Text), &seen); err != nil {
		t.Fatalf("unmarshal text: %v", err)
	}
	if len(seen.Rows) != 2 {
		t.Fatalf("LLM text rows: got %d, want 2 (preview cap)", len(seen.Rows))
	}
	if seen.RowCount != 5 {
		t.Fatalf("LLM text RowCount should stay 5 (true count), got %d", seen.RowCount)
	}
	if !seen.Truncated {
		t.Fatal("LLM text should be flagged Truncated so the model knows it's a sample")
	}
}

// TestEmit_LLMPreviewRows_DisabledByDefault asserts the default (0) is a no-op:
// the LLM sees the full set, byte-identical to pre-feature behavior.
func TestEmit_LLMPreviewRows_DisabledByDefault(t *testing.T) {
	exec := &executeSQLTool{sessionKey: "s"} // llmPreviewRows defaults to 0
	res := SQLResult{Columns: []string{"id"}, Rows: fiveRows(), RowCount: 5}

	out, err := exec.makeEmitFunc(context.Background())(res)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	var seen SQLResult
	if err := json.Unmarshal([]byte(out.Text), &seen); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(seen.Rows) != 5 || seen.Truncated {
		t.Fatalf("default must be a no-op: rows=%d truncated=%v", len(seen.Rows), seen.Truncated)
	}
}

// TestWithLLMPreviewRows_SetsField asserts the builder wires the cap through.
func TestWithLLMPreviewRows_SetsField(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil).WithLLMPreviewRows(50)
	if tool.llmPreviewRows != 50 {
		t.Fatalf("WithLLMPreviewRows: got %d, want 50", tool.llmPreviewRows)
	}
}
