package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

func maskEmail(col string, val any) any {
	if col == "email" {
		return "***"
	}
	return val
}

// TestEmit_CellRedactor_MasksTextNotHookOrStructured asserts WithCellRedactor
// masks ONLY the LLM-visible result text; the OnSQL hook (host grid), the
// tools.Result.Structured payload, and the caller's source row maps all keep
// full-fidelity values — i.e. the redaction deep-copies and never mutates the
// shared row maps.
func TestEmit_CellRedactor_MasksTextNotHookOrStructured(t *testing.T) {
	var hookEv SQLQueryEvent
	exec := &executeSQLTool{
		sessionKey:   "s",
		onSQL:        func(_ context.Context, ev SQLQueryEvent) { hookEv = ev },
		cellRedactor: maskEmail,
	}
	// "phone": nil exercises fn(col, nil) and the marshal of a redacted nil.
	row := map[string]any{"id": 1, "email": "alice@example.com", "phone": nil}
	res := SQLResult{SQL: "SELECT id, email, phone FROM users", Columns: []string{"id", "email", "phone"}, Rows: []map[string]any{row}, RowCount: 1}

	out, err := exec.makeEmitFunc(context.Background())(res)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	// Model-facing Text is masked.
	var seen SQLResult
	if err := json.Unmarshal([]byte(out.Text), &seen); err != nil {
		t.Fatalf("unmarshal text: %v", err)
	}
	if got := seen.Rows[0]["email"]; got != "***" {
		t.Fatalf("model-facing email should be masked, got %v", got)
	}
	if _, ok := seen.Rows[0]["id"]; !ok {
		t.Fatalf("non-sensitive column dropped from preview: %v", seen.Rows[0])
	}
	// A nil cell flows through the redactor and marshals without panicking.
	if got, ok := seen.Rows[0]["phone"]; !ok || got != nil {
		t.Fatalf("nil cell should pass through redactor as null, got %v (present=%v)", got, ok)
	}

	// Host hook keeps the real value (aliasing guard).
	if got := hookEv.Rows[0]["email"]; got != "alice@example.com" {
		t.Fatalf("OnSQL hook email must stay unmasked, got %v", got)
	}
	// Structured (host via OnToolResult) keeps the real value.
	if sr, ok := out.Structured.(SQLResult); !ok || sr.Rows[0]["email"] != "alice@example.com" {
		t.Fatalf("Structured email must stay unmasked, got %#v", out.Structured)
	}
	// The caller's source row map must not be mutated in place.
	if got := row["email"]; got != "alice@example.com" {
		t.Fatalf("source row map mutated by redactor: %v", got)
	}
}

// TestEmit_CellRedactor_ComposesWithPreview asserts redaction runs on the
// row-capped preview and still leaves the shared source maps untouched even on
// the truncating branch (where the preview shares the underlying maps).
func TestEmit_CellRedactor_ComposesWithPreview(t *testing.T) {
	r0 := map[string]any{"id": 0, "email": "a@x.com"}
	r1 := map[string]any{"id": 1, "email": "b@x.com"}
	res := SQLResult{Columns: []string{"id", "email"}, Rows: []map[string]any{r0, r1}, RowCount: 2}
	exec := &executeSQLTool{sessionKey: "s", llmPreviewRows: 1, cellRedactor: maskEmail}

	out, err := exec.makeEmitFunc(context.Background())(res)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	var seen SQLResult
	if err := json.Unmarshal([]byte(out.Text), &seen); err != nil {
		t.Fatalf("unmarshal text: %v", err)
	}
	if len(seen.Rows) != 1 {
		t.Fatalf("preview should cap to 1 row, got %d", len(seen.Rows))
	}
	if seen.Rows[0]["email"] != "***" {
		t.Fatalf("preview email not masked: %v", seen.Rows[0]["email"])
	}
	if !seen.Truncated {
		t.Fatal("preview should be flagged Truncated")
	}
	// Source maps (shared with the host grid / Structured) untouched.
	if r0["email"] != "a@x.com" || r1["email"] != "b@x.com" {
		t.Fatalf("source rows mutated: r0=%v r1=%v", r0["email"], r1["email"])
	}
}

// TestEmit_CellRedactor_NilIsNoOp asserts the default (no redactor) passes
// values through unchanged, byte-identical to pre-feature behavior.
func TestEmit_CellRedactor_NilIsNoOp(t *testing.T) {
	exec := &executeSQLTool{sessionKey: "s"} // cellRedactor nil
	res := SQLResult{Columns: []string{"email"}, Rows: []map[string]any{{"email": "real@x.com"}}, RowCount: 1}

	out, err := exec.makeEmitFunc(context.Background())(res)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	var seen SQLResult
	if err := json.Unmarshal([]byte(out.Text), &seen); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if seen.Rows[0]["email"] != "real@x.com" {
		t.Fatalf("nil redactor must be a no-op, got %v", seen.Rows[0]["email"])
	}
}

// TestWithCellRedactor_SetsField asserts the builder wires the redactor through.
func TestWithCellRedactor_SetsField(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil).WithCellRedactor(maskEmail)
	if tool.cellRedactor == nil {
		t.Fatal("WithCellRedactor did not set the redactor")
	}
}
