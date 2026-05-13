package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

func TestCallSQLAgentTool_DefaultIdentity(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil)
	if got := tool.Name(); got != "call_sql_agent" {
		t.Fatalf("Name default: got %q, want %q", got, "call_sql_agent")
	}
	if !tool.RequiresConfirmation() {
		t.Fatalf("RequiresConfirmation default: got false, want true")
	}
	if got := tool.Display().Label; got != "call_sql_agent" {
		t.Fatalf("Display default label: got %q, want %q", got, "call_sql_agent")
	}
}

func TestCallSQLAgentTool_WithName(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil).WithName("query_external_creatives")
	if got := tool.Name(); got != "query_external_creatives" {
		t.Fatalf("WithName: got %q, want %q", got, "query_external_creatives")
	}
	// Default Display should reflect the new name.
	if got := tool.Display().Label; got != "query_external_creatives" {
		t.Fatalf("Display label after WithName: got %q, want %q", got, "query_external_creatives")
	}
}

func TestCallSQLAgentTool_WithDisplay(t *testing.T) {
	custom := tools.ToolDisplay{Label: "Querying creatives", Category: "analytics", IconHint: "database"}
	tool := NewCallSQLAgentTool(nil, "", nil, nil).WithDisplay(custom)
	got := tool.Display()
	if got.Label != custom.Label || got.Category != custom.Category || got.IconHint != custom.IconHint {
		t.Fatalf("WithDisplay: got %+v, want %+v", got, custom)
	}
}

func TestCallSQLAgentTool_WithRequiresConfirmation(t *testing.T) {
	tool := NewCallSQLAgentTool(nil, "", nil, nil).WithRequiresConfirmation(false)
	if tool.RequiresConfirmation() {
		t.Fatalf("WithRequiresConfirmation(false): got true, want false")
	}
	tool.WithRequiresConfirmation(true)
	if !tool.RequiresConfirmation() {
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
	// Mutation-off (default) — Execute must refuse INSERT/UPDATE/DELETE
	// before touching the DB. We pass db=nil to prove no DB call happens
	// on the rejection path.
	exec := &executeSQLTool{db: nil, allowMutations: false}
	out, err := exec.Execute(context.Background(), `{"sql_query":"UPDATE customers SET active = false WHERE id = 1"}`)
	if err != nil {
		t.Fatalf("Execute returned a hard error instead of a structured rejection: %v", err)
	}
	if !strings.Contains(out, "not permitted") || !strings.Contains(out, "WithAllowMutations") {
		t.Fatalf("rejection payload should mention permission + the enabling builder, got %s", out)
	}
}

func TestExecuteSQLTool_DescriptionReflectsMutationFlag(t *testing.T) {
	off := (&executeSQLTool{allowMutations: false}).Description()
	on := (&executeSQLTool{allowMutations: true}).Description()
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
	if tool.Name() != "custom" || tool.Display().Label != "Custom Label" || tool.RequiresConfirmation() {
		t.Fatalf("chained builders: got name=%q label=%q confirm=%v", tool.Name(), tool.Display().Label, tool.RequiresConfirmation())
	}
}
