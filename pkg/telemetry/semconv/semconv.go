// Package semconv holds the OpenTelemetry attribute keys and metric names shared
// by the gopheragent instrumentation packages (otelllm, oteltools, and the agent
// loop). Centralizing them keeps span attributes and metric labels consistent
// across producers so a single trace or dashboard reads coherently.
//
// Naming follows the OpenTelemetry GenAI semantic conventions where they apply
// (gen_ai.*); gopheragent-specific series use the gopheragent.* prefix.
//
// This package imports only go.opentelemetry.io/otel/attribute — it never pulls
// the SDK, so importing it is free for a library consumer.
package semconv

import (
	"os"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

// ScopeName is the instrumentation scope reported on all gopheragent telemetry.
const ScopeName = "github.com/hung12ct/gopheragent"

// GenAI span/metric attribute keys (OpenTelemetry GenAI semantic conventions).
const (
	// GenAISystem identifies the model vendor, e.g. "anthropic", "openai", "gemini".
	// Deprecated upstream in favor of GenAIProviderName; emitted only under
	// SchemaLegacy.
	GenAISystem = attribute.Key("gen_ai.system")
	// GenAIProviderName is the current semconv name for the model vendor.
	GenAIProviderName = attribute.Key("gen_ai.provider.name")
	// GenAIAgentName is the human-readable agent name on an invoke_agent span.
	GenAIAgentName = attribute.Key("gen_ai.agent.name")
	// GenAIConversationID identifies the conversation; gopheragent sets it to
	// the session key.
	GenAIConversationID = attribute.Key("gen_ai.conversation.id")
	// GenAIRequestModel is the requested model name, e.g. "claude-opus-4-8".
	GenAIRequestModel = attribute.Key("gen_ai.request.model")
	// GenAIOperationName names the operation (OperationChat, OperationInvokeAgent).
	GenAIOperationName = attribute.Key("gen_ai.operation.name")
	// GenAIUsageInputTokens is the prompt token count reported by the provider.
	GenAIUsageInputTokens = attribute.Key("gen_ai.usage.input_tokens")
	// GenAIUsageOutputTokens is the completion token count reported by the provider.
	GenAIUsageOutputTokens = attribute.Key("gen_ai.usage.output_tokens")
	// GenAITokenType labels the token-usage metric stream as input or output.
	GenAITokenType = attribute.Key("gen_ai.token.type")
	// GenAIToolName is the tool being invoked.
	GenAIToolName = attribute.Key("gen_ai.tool.name")
	// GenAIToolCallID is the loop-generated correlation ID for a tool dispatch.
	GenAIToolCallID = attribute.Key("gen_ai.tool.call.id")
)

// gopheragent-specific attribute keys.
const (
	// SessionKey is the agent session identifier carried on the run and
	// iteration spans — the attribute to filter a conversation's traces by.
	SessionKey = attribute.Key("gopheragent.session.key")
	// Iteration is the zero-based ReAct iteration index on an iteration span.
	Iteration = attribute.Key("gopheragent.iteration")
	// RunOutcome is the closed-enum terminal outcome of a run (RunOutcome*
	// values), on the run span and the gopheragent.agent.runs counter.
	RunOutcome = attribute.Key("gopheragent.run.outcome")
	// HITLDecision is the human-approval result on a hitl.await_approval span:
	// "approved", "denied", "timed_out", or "cancelled" (run stopped mid-wait).
	HITLDecision = attribute.Key("gopheragent.hitl.decision")
)

// RunOutcome values. A closed set keeps the runs counter low-cardinality.
const (
	RunOutcomeDone            = "done"
	RunOutcomeMaxIterations   = "max_iterations"
	RunOutcomeToolCallCap     = "tool_call_cap"
	RunOutcomeLoopDetected    = "loop_detected"
	RunOutcomeCancelled       = "cancelled"
	RunOutcomeHookRejected    = "hook_rejected"
	RunOutcomeApprovalPending = "approval_pending"
	RunOutcomeLLMError        = "llm_error"
	RunOutcomeError           = "error"
	RunOutcomeUnknown         = "unknown"
)

// GenAI operation values.
const (
	// OperationChat is the gen_ai.operation.name value for a chat completion.
	OperationChat = "chat"
	// OperationInvokeAgent is the gen_ai.operation.name value for an agent run.
	OperationInvokeAgent = "invoke_agent"
)

// Schema selects which revision of the GenAI semantic conventions the
// instrumentation emits. GenAI semconv is still experimental and renames
// attributes and span names between releases; the schema lets dashboards
// migrate on their own schedule instead of breaking on a library upgrade.
type Schema uint8

const (
	// SchemaLegacy (the default) keeps the historical span names (agent.run)
	// and metric labels (gen_ai.system), and additionally stamps the current
	// semconv attributes on spans so GenAI-aware backends recognize them.
	SchemaLegacy Schema = iota
	// SchemaLatest emits only the current semconv: "invoke_agent <name>" span
	// names and gen_ai.provider.name in place of gen_ai.system.
	SchemaLatest
)

// EnvStabilityOptIn is the standard OpenTelemetry opt-in variable; listing
// OptInGenAILatest in it (comma-separated) selects SchemaLatest.
const (
	EnvStabilityOptIn = "OTEL_SEMCONV_STABILITY_OPT_IN"
	OptInGenAILatest  = "gen_ai_latest_experimental"
)

// SchemaFromEnv resolves the schema from OTEL_SEMCONV_STABILITY_OPT_IN. Call it
// at construction time, never per span: instrumentation resolves it once and
// keeps the result.
func SchemaFromEnv() Schema {
	for _, v := range strings.Split(os.Getenv(EnvStabilityOptIn), ",") {
		if strings.TrimSpace(v) == OptInGenAILatest {
			return SchemaLatest
		}
	}
	return SchemaLegacy
}

// Token-type values for the GenAITokenType label.
const (
	// TokenTypeInput labels prompt tokens.
	TokenTypeInput = "input"
	// TokenTypeOutput labels completion tokens.
	TokenTypeOutput = "output"
)

// DurationBucketsSeconds is the explicit histogram bucket boundaries (in
// seconds) for all gopheragent duration histograms — LLM calls, tool
// executions, and iterations. It follows the OpenTelemetry GenAI semantic
// convention for gen_ai.client.operation.duration: an exponential ladder from
// 10 ms to ~82 s that gives real resolution across LLM-scale latencies. The
// SDK-default buckets (0, 5, 10, 25, … 10000) assume milliseconds and collapse
// every sub-5-second call into one bucket, making p50/p95 meaningless for
// second-scale LLM traffic. Attached as an instrument advisory so it stays in
// the API-only core; consumers can still override via an SDK View.
var DurationBucketsSeconds = []float64{
	0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92,
}

// Metric instrument names.
const (
	// MetricLLMDuration is the LLM call latency histogram (seconds).
	MetricLLMDuration = "gen_ai.client.operation.duration"
	// MetricLLMTokenUsage is the token-usage counter, split by GenAITokenType.
	MetricLLMTokenUsage = "gen_ai.client.token.usage"
	// MetricToolDuration is the tool execution latency histogram (seconds).
	MetricToolDuration = "gopheragent.tool.execution.duration"
	// MetricToolErrors counts failed tool executions.
	MetricToolErrors = "gopheragent.tool.errors"
	// MetricIterationDuration is the ReAct iteration latency histogram (seconds).
	MetricIterationDuration = "gopheragent.agent.iteration.duration"
	// MetricAgentRuns counts finished runs by RunOutcome and gen_ai.agent.name.
	MetricAgentRuns = "gopheragent.agent.runs"
	// MetricAgentCost sums run cost in USD by gen_ai.agent.name.
	MetricAgentCost = "gopheragent.agent.cost"
)
