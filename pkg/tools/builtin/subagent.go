package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// SubAgentInput is the JSON-decoded argument schema for CallSubAgentTool.
type SubAgentInput struct {
	TaskDescription string `json:"task_description"`
	AgentName       string `json:"agent_name"`
}

// CallSubAgentTool delegates a task to an isolated worker sub-agent with its own
// session state, preventing context-window pollution in the main agent.
type CallSubAgentTool struct {
	Sessions agent.SessionManager
	Tools    *tools.Registry
	LLM      agent.LLMProvider
}

// NewCallSubAgentTool constructs a CallSubAgentTool wired to the given dependencies.
func NewCallSubAgentTool(sessions agent.SessionManager, registry *tools.Registry, llm agent.LLMProvider) *CallSubAgentTool {
	return &CallSubAgentTool{
		Sessions: sessions,
		Tools:    registry,
		LLM:      llm,
	}
}

const callSubAgentName = "call_sub_agent"
const callSubAgentDescription = "Delegates a complex reasoning or multi-step task to a specialized worker sub-agent. The sub-agent operates in a completely isolated context and returns a final structured report. Useful to avoid context window explosions."

func (t *CallSubAgentTool) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        callSubAgentName,
		Description: callSubAgentDescription,
		Parameters: tools.ToolSchema{
			Type: "object",
			Properties: map[string]any{
				"task_description": map[string]any{
					"type":        "string",
					"description": "Detailed instructions for the worker sub-agent.",
				},
				"agent_name": map[string]any{
					"type":        "string",
					"description": "Name of the worker persona (used to namespace the worker's session and tag its report).",
				},
			},
			Required: []string{"task_description", "agent_name"},
		},
		Display: tools.DefaultDisplay(callSubAgentName, callSubAgentDescription),
	}
}

// Execute runs the sub-agent in an isolated session and returns its final report.
// The worker session is always deleted before returning, regardless of outcome,
// so no orphan history remains in the backing SessionManager.
//
// When the invoking agent loop has installed a sub-agent emitter on ctx (it
// does so for every tool invocation), the worker is run in streaming mode and
// its events — thoughts, tool calls, tool progress, content chunks, errors —
// are forwarded to the parent stream tagged with Source="subagent:<name>" and
// ParentID=<parent session key>. This makes the parent no longer look frozen
// while the sub-agent works, and gives UIs enough metadata to render a nested
// activity timeline. When no emitter is present (e.g. tests calling Execute
// directly), Execute falls back to the original non-streaming path.
func (t *CallSubAgentTool) Execute(ctx context.Context, inputJSON string) (tools.Result, error) {
	var input SubAgentInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return tools.Result{}, fmt.Errorf("tools: invalid json: %w", err)
	}

	workerSessionKey := fmt.Sprintf("subagent-%s-%d", input.AgentName, time.Now().UnixNano())
	defer func() {
		// Always clean up the worker's ephemeral session; ignore error —
		// the tool's primary result is more important than cleanup failure.
		_ = t.Sessions.Delete(context.Background(), workerSessionKey)
	}()

	workerLoop := agent.NewAgentLoop(t.Sessions, t.Tools, t.LLM)
	workerLoop.MaxIters = 15
	workerLoop.EmitThoughts = true // forward thoughts upstream when streaming; parent filters as needed
	workerLoop.DynamicContext = agent.DynamicContextFuncFromContext(ctx)
	workerLoop.ConfirmHITL = agent.ConfirmHITLFromContext(ctx)
	workerLoop.ConfirmHITLTimeout = agent.ConfirmHITLTimeoutFromContext(ctx)

	instruction := fmt.Sprintf("[%s Sub-Agent Task]\n%s", input.AgentName, input.TaskDescription)

	emitter := agent.SubAgentEmitterFromContext(ctx)
	if emitter == nil {
		// No parent stream to forward into — keep the legacy non-streaming path.
		workerLoop.EmitThoughts = false
		result, err := workerLoop.RunIteration(ctx, workerSessionKey, instruction)
		if err != nil {
			return tools.Result{}, fmt.Errorf("tools: sub-agent %s failed: %w", input.AgentName, err)
		}
		return tools.Text(fmt.Sprintf("Report from %s:\n%s", input.AgentName, result)), nil
	}

	parentSessionKey, _ := agent.SessionKeyFromContext(ctx)
	source := "subagent:" + input.AgentName

	var buf strings.Builder
	var workerErr error
	var hitlEvent agent.StreamEvent
	for ev := range workerLoop.RunText(ctx, workerSessionKey, instruction) {
		// Accumulate the worker's own content as its final report; content that
		// comes from deeper nested sub-agents (Source already set) is only
		// observational and must not pollute this worker's answer.
		if ev.Source == "" {
			switch p := ev.Payload.(type) {
			case agent.ContentEvent:
				buf.WriteString(p.Text)
			case agent.ErrorEvent:
				if workerErr == nil {
					if p.Err != nil {
						workerErr = p.Err
					} else if p.Message != "" {
						workerErr = fmt.Errorf("agent: %s", p.Message)
					}
				}
			case agent.HITLDeniedEvent, agent.HITLTimedOutEvent:
				// Latch the most recent HITL signal so the report surfaces
				// the cause verbatim instead of relying on the worker's
				// paraphrase.
				hitlEvent = ev
			}
		}

		emitter(agent.DecorateForwardedEvent(ev, source, parentSessionKey))
	}

	if workerErr != nil {
		return tools.Result{}, fmt.Errorf("tools: sub-agent %s failed: %w", input.AgentName, workerErr)
	}

	return tools.Text(fmt.Sprintf("Report from %s:\n%s", input.AgentName, subAgentHITLReport(hitlEvent, buf.String()))), nil
}

// subAgentHITLReport mirrors hitlBlockedReport for CallSubAgentTool: when
// the worker's HITL gate fired, prepend an explicit "HITL_BLOCKED" directive
// so the outer agent sees the cause directly rather than the worker's
// paraphrased final answer.
func subAgentHITLReport(hitlEvent agent.StreamEvent, innerSummary string) string {
	switch p := hitlEvent.Payload.(type) {
	case agent.HITLTimedOutEvent:
		return fmt.Sprintf("HITL_BLOCKED: timeout — the approval prompt for tool %q expired after %s; the user did NOT refuse. STOP. Do NOT retry automatically. Do NOT rephrase or re-issue the same call. Reply to the user with one short paragraph saying the approval window closed and ask them to confirm when ready, then end your turn. Worker summary (may be misleading): %s", p.Tool, p.Timeout, innerSummary)
	case agent.HITLDeniedEvent:
		return fmt.Sprintf("HITL_BLOCKED: denied — the operator denied approval for tool %q. STOP. Do NOT call this tool again with the same or similar arguments. Do NOT retry, rephrase, or work around the gate. Reply to the user with one short paragraph stating the request was not approved and ask whether they have an alternative approach, then end your turn. Worker summary (may be misleading): %s", p.Tool, innerSummary)
	default:
		return innerSummary
	}
}
