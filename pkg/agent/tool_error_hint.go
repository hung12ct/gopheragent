package agent

import "fmt"

// ToolErrorHintFormatter shapes a raw tool-execution error into the text
// written back to the model as the tool_result content. Because the model
// re-reads history on the next turn, the wording has a direct effect on
// whether it recovers: a flat "Error: ..." string gives the model no
// scaffolding, while a structured hint ("the tool failed with X; retry
// with corrected arguments or abandon the plan") materially improves
// first-retry success rates.
//
// The default is defaultToolErrorHint. Override on AgentLoop to inject
// domain knowledge — e.g. mapping SQL driver errors to remediation
// suggestions, or stripping noisy traces before they reach the model.
type ToolErrorHintFormatter func(toolName, errMsg string) string

// defaultToolErrorHint wraps a raw tool error into a structured correction
// hint. Keep the format short — the model pays input-token cost to re-read
// every tool result on every subsequent turn.
func defaultToolErrorHint(toolName, errMsg string) string {
	return fmt.Sprintf(
		"[TOOL_ERROR] The tool `%s` failed:\n%s\n\n"+
			"This was a tool-side failure, not a model error. "+
			"Analyze the error message, correct the arguments, and retry. "+
			"If the error indicates the request is impossible or the tool is unavailable, "+
			"stop calling this tool and report the limitation to the user.",
		toolName, errMsg,
	)
}

// formatToolError returns the tool-result content the loop should write
// when a tool execution fails. It applies the loop's ToolErrorHintFormatter
// when set and falls back to the default — keeping call sites branch-free.
func (al *AgentLoop) formatToolError(toolName, errMsg string) string {
	fn := al.ToolErrorHintFormatter
	if fn == nil {
		fn = defaultToolErrorHint
	}
	return fn(toolName, errMsg)
}
