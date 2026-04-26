package agent

import "fmt"

// ToolErrorContext carries the full execution context of a failed tool
// invocation so a custom formatter can build messages aware of the args
// the model sent, which iteration the failure happened in, and the
// underlying error (suitable for errors.Is/As checks).
type ToolErrorContext struct {
	ToolName  string
	ArgsJSON  string
	Iteration int
	Cause     error
}

// ToolErrorHintFormatter shapes a failed tool invocation into the text
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
type ToolErrorHintFormatter func(ToolErrorContext) string

func defaultToolErrorHint(ctx ToolErrorContext) string {
	return fmt.Sprintf(
		"[TOOL_ERROR] The tool `%s` failed:\n%s\n\n"+
			"This was a tool-side failure, not a model error. "+
			"Analyze the error message, correct the arguments, and retry. "+
			"If the error indicates the request is impossible or the tool is unavailable, "+
			"stop calling this tool and report the limitation to the user.",
		ctx.ToolName, ctx.Cause,
	)
}

func (al *AgentLoop) formatToolError(ctx ToolErrorContext) string {
	fn := al.ToolErrorHintFormatter
	if fn == nil {
		fn = defaultToolErrorHint
	}
	return fn(ctx)
}
