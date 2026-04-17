// Package history provides message types and session storage backends for agent conversations.
package history

// Message represents a single message in a chat conversation.
// It supports all standard roles: "system", "user", "assistant", and "tool".
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // for role:"tool" result messages
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // for role:"assistant" messages with tool calls
	IsError    bool       `json:"is_error,omitempty"`     // true when a tool execution failed
}

// ToolCall represents a tool invocation declared by the assistant.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string
}

// Session represents a chat session with its full message history.
type Session struct {
	Key        string               `json:"key"`
	Messages   []Message            `json:"messages"`
	AsyncTasks map[string]AsyncTask `json:"async_tasks,omitempty"`
}

// AsyncTask represents a background task managed by the session.
type AsyncTask struct {
	TaskID    string `json:"task_id"`
	AgentName string `json:"agent_name"`
	Status    string `json:"status"` // "running", "success", "error", "cancelled", "interrupted"
	Result    string `json:"result,omitempty"`
}
