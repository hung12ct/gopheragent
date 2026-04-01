package main

import (
	"context"
	"fmt"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// DeleteSystemTool is a mock high-risk tool that requires human approval before executing.
type DeleteSystemTool struct{}

func NewDeleteSystemTool() *DeleteSystemTool {
	return &DeleteSystemTool{}
}

func (t *DeleteSystemTool) Name() string {
	return "delete_database_records"
}

func (t *DeleteSystemTool) Description() string {
	return "Deletes records from the main database. This is a HIGH RISK operation."
}

func (t *DeleteSystemTool) RequiresConfirmation() bool {
	return true // This tells the AgentLoop to pause and ask the human
}

func (t *DeleteSystemTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]interface{}{
			"table":     map[string]interface{}{"type": "string", "description": "The table to delete from"},
			"condition": map[string]interface{}{"type": "string", "description": "The WHERE condition for deletion"},
		},
		Required: []string{"table", "condition"},
	}
}

func (t *DeleteSystemTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	// In a real scenario, this would execute the DELETE statement
	return fmt.Sprintf("Successfully deleted records with args: %s", argsJSON), nil
}
