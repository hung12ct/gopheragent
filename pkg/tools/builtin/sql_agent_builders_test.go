package builtin

import (
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

func TestSQLAgentTool_DefaultIdentity(t *testing.T) {
	tool := NewSQLAgentTool(nil, "", nil, nil)
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

func TestSQLAgentTool_WithName(t *testing.T) {
	tool := NewSQLAgentTool(nil, "", nil, nil).WithName("query_external_creatives")
	if got := tool.Name(); got != "query_external_creatives" {
		t.Fatalf("WithName: got %q, want %q", got, "query_external_creatives")
	}
	// Default Display should reflect the new name.
	if got := tool.Display().Label; got != "query_external_creatives" {
		t.Fatalf("Display label after WithName: got %q, want %q", got, "query_external_creatives")
	}
}

func TestSQLAgentTool_WithDisplay(t *testing.T) {
	custom := tools.ToolDisplay{Label: "Querying creatives", Category: "analytics", IconHint: "database"}
	tool := NewSQLAgentTool(nil, "", nil, nil).WithDisplay(custom)
	got := tool.Display()
	if got.Label != custom.Label || got.Category != custom.Category || got.IconHint != custom.IconHint {
		t.Fatalf("WithDisplay: got %+v, want %+v", got, custom)
	}
}

func TestSQLAgentTool_WithRequiresConfirmation(t *testing.T) {
	tool := NewSQLAgentTool(nil, "", nil, nil).WithRequiresConfirmation(false)
	if tool.RequiresConfirmation() {
		t.Fatalf("WithRequiresConfirmation(false): got true, want false")
	}
	tool.WithRequiresConfirmation(true)
	if !tool.RequiresConfirmation() {
		t.Fatalf("WithRequiresConfirmation(true): got false, want true")
	}
}

func TestSQLAgentTool_BuildersChain(t *testing.T) {
	tool := NewSQLAgentTool(nil, "", nil, nil).
		WithName("custom").
		WithDisplay(tools.ToolDisplay{Label: "Custom Label"}).
		WithRequiresConfirmation(false)
	if tool.Name() != "custom" || tool.Display().Label != "Custom Label" || tool.RequiresConfirmation() {
		t.Fatalf("chained builders: got name=%q label=%q confirm=%v", tool.Name(), tool.Display().Label, tool.RequiresConfirmation())
	}
}
