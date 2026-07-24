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

import "go.opentelemetry.io/otel/attribute"

// GenAI span/metric attribute keys (OpenTelemetry GenAI semantic conventions).
const (
	// GenAISystem identifies the model vendor, e.g. "anthropic", "openai", "gemini".
	GenAISystem = attribute.Key("gen_ai.system")
	// GenAIRequestModel is the requested model name, e.g. "claude-opus-4-8".
	GenAIRequestModel = attribute.Key("gen_ai.request.model")
	// GenAIOperationName names the operation; gopheragent uses OperationChat.
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
	// SessionKey is the agent session identifier carried on iteration spans.
	SessionKey = attribute.Key("gopheragent.session.key")
	// Iteration is the zero-based ReAct iteration index on an iteration span.
	Iteration = attribute.Key("gopheragent.iteration")
)

// GenAI operation values.
const (
	// OperationChat is the gen_ai.operation.name value for a chat completion.
	OperationChat = "chat"
)

// Token-type values for the GenAITokenType label.
const (
	// TokenTypeInput labels prompt tokens.
	TokenTypeInput = "input"
	// TokenTypeOutput labels completion tokens.
	TokenTypeOutput = "output"
)

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
)
