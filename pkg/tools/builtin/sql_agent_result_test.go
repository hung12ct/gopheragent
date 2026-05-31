package builtin

import (
	"strings"
	"testing"
)

// TestSQLAgentResult_AttachesStructuredWhenPresent verifies the sub-agent's
// natural-language answer is the model-visible Text and the captured
// SQLResult rides on Structured for host integrations.
func TestSQLAgentResult_AttachesStructuredWhenPresent(t *testing.T) {
	sr := &SQLResult{Columns: []string{"id"}, Rows: []map[string]any{{"id": 1}}, RowCount: 1}
	res := sqlAgentResult("found 1 row", sr)

	if res.Text != "found 1 row" {
		t.Fatalf("Text: got %q, want %q", res.Text, "found 1 row")
	}
	got, ok := res.Structured.(*SQLResult)
	if !ok {
		t.Fatalf("Structured: got %T, want *SQLResult", res.Structured)
	}
	if got != sr {
		t.Fatalf("Structured: got %p, want %p", got, sr)
	}
}

// TestSQLAgentResult_NilResultLeavesStructuredNil guards the typed-nil trap:
// a nil *SQLResult must NOT be boxed into the `any` field (which would make
// `res.Structured != nil` true for adopters and break their guards).
func TestSQLAgentResult_NilResultLeavesStructuredNil(t *testing.T) {
	res := sqlAgentResult("no query succeeded", nil)

	if res.Structured != nil {
		t.Fatalf("Structured: got %#v (type %T), want untyped nil", res.Structured, res.Structured)
	}
}

// TestCallSQLAgentTool_DescriptionDoesNotClaimRawRows locks in the honest
// contract: the tool returns a written answer, not raw/exportable rows. A
// misleading "returns structured data" claim is what drove a parent agent to
// loop trying to coax verbatim rows out of it for a CSV export.
func TestCallSQLAgentTool_DescriptionDoesNotClaimRawRows(t *testing.T) {
	desc := NewCallSQLAgentTool(nil, "", nil, nil).Descriptor().Description
	if strings.Contains(desc, "returns structured data") {
		t.Fatalf("description still makes the misleading structured-data claim: %q", desc)
	}
	if !strings.Contains(desc, "not raw table rows") {
		t.Fatalf("description should state it does not return raw rows: %q", desc)
	}
}
