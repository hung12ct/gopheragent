package agent

import (
	"context"
	"strings"
	"time"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// MockProvider is used exclusively for examples and testing without an API Key.
// It hardcodes responses based on the tool requested to simulate a real LLM.
type MockProvider struct {
	IterationCounter int
}

// NewMockProvider creates a mock provider for testing and examples.
func NewMockProvider() *MockProvider {
	return &MockProvider{IterationCounter: 0}
}

func (m *MockProvider) GenerateStream(ctx context.Context, memory []history.Message, registry *tools.Registry, streamChan chan<- StreamEvent) (LLMResult, error) {
	m.IterationCounter++

	var lastUserInput string
	for i := len(memory) - 1; i >= 0; i-- {
		if memory[i].Role == "user" {
			lastUserInput = memory[i].Content
			break
		}
	}

	time.Sleep(1 * time.Second) // Simulate API latency

	// Direct chat
	if strings.Contains(strings.ToLower(lastUserInput), "hello") || strings.Contains(strings.ToLower(lastUserInput), "chào") {
		streamChan <- StreamEvent{Type: EventTypeContent, Content: "Hello! I am ready to process Data requests."}
		return LLMResult{Content: "Hello! I am ready to process Data requests."}, nil
	}

	// First turn: pretend LLM decided to call a tool
	if m.IterationCounter == 1 {
		toolName := "call_sql_agent"
		argsJSON := `{"query": "Fetch analytics..."}`

		if strings.Contains(strings.ToLower(lastUserInput), "delete") || strings.Contains(strings.ToLower(lastUserInput), "xóa") {
			toolName = "delete_database_records"
			argsJSON = `{"table": "users", "condition": "all"}`
		}
		
		// If running inside SQL Sub-Agent, it should call execute_sql instead of call_sql_agent
		if _, hasExecuteSQL := registry.Get("execute_sql"); hasExecuteSQL {
			toolName = "execute_sql"
			argsJSON = `{"sql_query": "SELECT * FROM mock_db LIMIT 10"}`
		}

		streamChan <- StreamEvent{Type: EventTypeThought, Content: "Analyzing request constraints..."}
		time.Sleep(300 * time.Millisecond)

		return LLMResult{
			ToolCalls: []PendingToolCall{
				{ID: "mock_call_1", Name: toolName, ArgsJSON: argsJSON},
			},
		}, nil
	}

	// Second turn: finalize response after tool returned data
	finalOutput := "Here is the summarized result based on the database extraction perfectly charting your parameters."
	streamChan <- StreamEvent{Type: EventTypeContent, Content: finalOutput}
	return LLMResult{Content: finalOutput}, nil
}
