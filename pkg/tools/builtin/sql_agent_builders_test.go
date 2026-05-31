package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

func TestCallSQLAgentTool_DefaultIdentity(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil)
	desc := tool.Descriptor()
	if desc.Name != "call_sql_agent" {
		t.Fatalf("Name default: got %q, want %q", desc.Name, "call_sql_agent")
	}
	if !desc.RequiresConfirmation {
		t.Fatalf("RequiresConfirmation default: got false, want true")
	}
	if desc.Display.Label != "call_sql_agent" {
		t.Fatalf("Display default label: got %q, want %q", desc.Display.Label, "call_sql_agent")
	}
}

func TestCallSQLAgentTool_WithName(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil).WithName("query_external_creatives")
	desc := tool.Descriptor()
	if desc.Name != "query_external_creatives" {
		t.Fatalf("WithName: got %q, want %q", desc.Name, "query_external_creatives")
	}
	if desc.Display.Label != "query_external_creatives" {
		t.Fatalf("Display label after WithName: got %q, want %q", desc.Display.Label, "query_external_creatives")
	}
}

func TestCallSQLAgentTool_WithDisplay(t *testing.T) {
	custom := tools.ToolDisplay{Label: "Querying creatives", Category: "analytics", IconHint: "database"}
	tool := NewCallSQLAgentTool(nil, "", nil, nil).WithDisplay(custom)
	got := tool.Descriptor().Display
	if got.Label != custom.Label || got.Category != custom.Category || got.IconHint != custom.IconHint {
		t.Fatalf("WithDisplay: got %+v, want %+v", got, custom)
	}
}

func TestCallSQLAgentTool_WithRequiresConfirmation(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil).WithRequiresConfirmation(false)
	if tool.Descriptor().RequiresConfirmation {
		t.Fatalf("WithRequiresConfirmation(false): got true, want false")
	}
	tool.WithRequiresConfirmation(true)
	if !tool.Descriptor().RequiresConfirmation {
		t.Fatalf("WithRequiresConfirmation(true): got false, want true")
	}
}

func TestCallSQLAgentTool_WithAllowMutations(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil)
	if tool.allowMutations {
		t.Fatalf("WithAllowMutations default: got true, want false")
	}
	tool.WithAllowMutations(true)
	if !tool.allowMutations {
		t.Fatalf("WithAllowMutations(true): flag not set")
	}
	tool.WithAllowMutations(false)
	if tool.allowMutations {
		t.Fatalf("WithAllowMutations(false): flag not cleared")
	}
}

func TestExecuteSQLTool_RejectsMutationsWhenDisabled(t *testing.T) {
	exec := &executeSQLTool{db: nil, allowMutations: false}
	out, err := exec.Execute(context.Background(), `{"sql_query":"UPDATE customers SET active = false WHERE id = 1"}`)
	if err != nil {
		t.Fatalf("Execute returned a hard error instead of a structured rejection: %v", err)
	}
	if !strings.Contains(out.Text, "not permitted") || !strings.Contains(out.Text, "WithAllowMutations") {
		t.Fatalf("rejection payload should mention permission + the enabling builder, got %s", out.Text)
	}
}

func TestExecuteSQLTool_EmitSurfacesRowCountAndExecutionMs(t *testing.T) {
	var got SQLQueryEvent
	exec := &executeSQLTool{
		sessionKey: "sess_1",
		onSQL:      func(_ context.Context, ev SQLQueryEvent) { got = ev },
	}
	emit := exec.makeEmitFunc(context.Background())

	res := SQLResult{
		SQL:         "UPDATE customers SET active = false WHERE id = 1",
		RowCount:    3,
		ExecutionMs: 42,
	}
	out, err := emit(res)
	if err != nil {
		t.Fatalf("emit returned error: %v", err)
	}
	if got.RowCount != 3 {
		t.Fatalf("SQLQueryEvent.RowCount: got %d, want 3", got.RowCount)
	}
	if got.ExecutionMs != 42 {
		t.Fatalf("SQLQueryEvent.ExecutionMs: got %d, want 42", got.ExecutionMs)
	}
	// The model-facing payload must keep carrying the same numbers.
	if !strings.Contains(out.Text, `"row_count":3`) || !strings.Contains(out.Text, `"execution_ms":42`) {
		t.Fatalf("marshalled result should retain row_count/execution_ms, got %s", out.Text)
	}
}

func TestExecuteSQLTool_DescriptionReflectsMutationFlag(t *testing.T) {
	off := (&executeSQLTool{allowMutations: false}).Descriptor().Description
	on := (&executeSQLTool{allowMutations: true}).Descriptor().Description
	if !strings.Contains(off, "read-only") || strings.Contains(off, "INSERT") {
		t.Fatalf("read-only description regressed: %s", off)
	}
	if !strings.Contains(on, "INSERT") || !strings.Contains(on, "MERGE") {
		t.Fatalf("mutation-enabled description should list DML verbs: %s", on)
	}
}

func TestCallSQLAgentTool_BuildSystemPromptMentionsMutations(t *testing.T) {
	off := NewCallSQLAgentTool(nil, "schema", nil, nil).buildSystemPrompt()
	on := NewCallSQLAgentTool(nil, "schema", nil, nil).WithAllowMutations(true).buildSystemPrompt()
	if !strings.Contains(off, "read-only SQL query") || strings.Contains(off, "Mutations (INSERT") {
		t.Fatalf("read-only prompt regressed: %s", off)
	}
	if !strings.Contains(on, "Mutations (INSERT, UPDATE, DELETE, MERGE) are permitted") {
		t.Fatalf("mutation-enabled prompt missing rule 12: %s", on)
	}
}

func TestCallSQLAgentTool_BuildersChain(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil).
		WithName("custom").
		WithDisplay(tools.ToolDisplay{Label: "Custom Label"}).
		WithRequiresConfirmation(false)
	desc := tool.Descriptor()
	if desc.Name != "custom" || desc.Display.Label != "Custom Label" || desc.RequiresConfirmation {
		t.Fatalf("chained builders: got name=%q label=%q confirm=%v", desc.Name, desc.Display.Label, desc.RequiresConfirmation)
	}
}

func TestCallSQLAgentTool_WithExecuteSQLConfirmation(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil)
	if tool.execSQLRequiresConfirmation {
		t.Fatalf("WithExecuteSQLConfirmation default: got true, want false")
	}
	tool.WithExecuteSQLConfirmation(true)
	if !tool.execSQLRequiresConfirmation {
		t.Fatalf("WithExecuteSQLConfirmation(true): flag not set")
	}
	tool.WithExecuteSQLConfirmation(false)
	if tool.execSQLRequiresConfirmation {
		t.Fatalf("WithExecuteSQLConfirmation(false): flag not cleared")
	}
}

func TestCallSQLAgentTool_WithAllowDDL(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil)
	if tool.allowDDL {
		t.Fatalf("WithAllowDDL default: got true, want false")
	}
	tool.WithAllowDDL(true)
	if !tool.allowDDL {
		t.Fatalf("WithAllowDDL(true): flag not set")
	}
	tool.WithAllowDDL(false)
	if tool.allowDDL {
		t.Fatalf("WithAllowDDL(false): flag not cleared")
	}
}

func TestExecuteSQLTool_RejectsDDLWhenDisabled(t *testing.T) {
	exec := &executeSQLTool{db: nil, allowDDL: false}
	out, err := exec.Execute(context.Background(), `{"sql_query":"DROP TABLE customers"}`)
	if err != nil {
		t.Fatalf("Execute returned a hard error instead of a structured rejection: %v", err)
	}
	if !strings.Contains(out.Text, "DDL") || !strings.Contains(out.Text, "WithAllowDDL") {
		t.Fatalf("rejection payload should mention DDL + the enabling builder, got %s", out.Text)
	}
}

func TestExecuteSQLTool_DescriptionReflectsDDLFlag(t *testing.T) {
	off := (&executeSQLTool{allowDDL: false}).Descriptor().Description
	on := (&executeSQLTool{allowDDL: true}).Descriptor().Description
	both := (&executeSQLTool{allowMutations: true, allowDDL: true}).Descriptor().Description
	if strings.Contains(off, "CREATE") {
		t.Fatalf("DDL-off description should not advertise CREATE: %s", off)
	}
	if !strings.Contains(on, "CREATE") || !strings.Contains(on, "DROP") || !strings.Contains(on, "ALTER") {
		t.Fatalf("DDL-on description should list DDL verbs: %s", on)
	}
	if !strings.Contains(both, "INSERT") || !strings.Contains(both, "CREATE") {
		t.Fatalf("both-on description should list DML + DDL verbs: %s", both)
	}
}

func TestCallSQLAgentTool_BuildSystemPromptMentionsDDL(t *testing.T) {
	off := NewCallSQLAgentTool(nil, "schema", nil, nil).buildSystemPrompt()
	on := NewCallSQLAgentTool(nil, "schema", nil, nil).WithAllowDDL(true).buildSystemPrompt()
	if strings.Contains(off, "DDL (CREATE, DROP, ALTER") {
		t.Fatalf("DDL-off prompt regressed (mentions DDL as permitted): %s", off)
	}
	if !strings.Contains(on, "DDL (CREATE, DROP, ALTER") {
		t.Fatalf("DDL-on prompt missing rule 12: %s", on)
	}
}

func TestCallSQLAgentTool_WithProviderHintInjectsAddendum(t *testing.T) {
	prompt := NewCallSQLAgentTool(nil, "schema", nil, nil).
		WithAllowMutations(true).
		WithProviderHint("Do NOT propose a follow-up DELETE based on rows you just read.").
		buildSystemPrompt()
	if !strings.Contains(prompt, "Provider-specific guidance") {
		t.Fatalf("provider hint section missing: %s", prompt)
	}
	if !strings.Contains(prompt, "Do NOT propose a follow-up DELETE") {
		t.Fatalf("provider hint body missing: %s", prompt)
	}
	// Hint must sit between safety contract and schema block so it's read
	// as binding (not as schema metadata).
	contractIdx := strings.Index(prompt, "Safety contract")
	hintIdx := strings.Index(prompt, "Provider-specific guidance")
	schemaIdx := strings.Index(prompt, "Schema (use ONLY")
	if contractIdx >= hintIdx || hintIdx >= schemaIdx {
		t.Fatalf("provider hint out of order: contract=%d hint=%d schema=%d", contractIdx, hintIdx, schemaIdx)
	}
}

func TestCallSQLAgentTool_WithProviderHintEmptyOmitsSection(t *testing.T) {
	prompt := NewCallSQLAgentTool(nil, "schema", nil, nil).buildSystemPrompt()
	if strings.Contains(prompt, "Provider-specific guidance") {
		t.Fatalf("provider hint section should be absent when hint is empty: %s", prompt)
	}
}

func TestCallSQLAgentTool_WithAllowSelectStar(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil)
	if tool.allowSelectStar {
		t.Fatalf("WithAllowSelectStar default: got true, want false")
	}
	tool.WithAllowSelectStar(true)
	if !tool.allowSelectStar {
		t.Fatalf("WithAllowSelectStar(true): flag not set")
	}
}

func TestExecuteSQLTool_RejectsBareSelectStar(t *testing.T) {
	exec := &executeSQLTool{db: nil, allowSelectStar: false}
	out, err := exec.Execute(context.Background(), `{"sql_query":"SELECT * FROM customers"}`)
	if err != nil {
		t.Fatalf("Execute returned a hard error instead of a structured rejection: %v", err)
	}
	if !strings.Contains(out.Text, "bare 'SELECT *'") || !strings.Contains(out.Text, "WithAllowSelectStar") {
		t.Fatalf("rejection payload should mention the rule + the enabling builder, got %s", out.Text)
	}
}

func TestExecuteSQLTool_RequiresConfirmationReflectsFlag(t *testing.T) {
	off := (&executeSQLTool{}).Descriptor().RequiresConfirmation
	on := (&executeSQLTool{requiresConfirmation: true}).Descriptor().RequiresConfirmation
	if off {
		t.Fatalf("default executeSQLTool.RequiresConfirmation should be false, got true")
	}
	if !on {
		t.Fatalf("executeSQLTool.RequiresConfirmation should follow the flag, got false")
	}
}
