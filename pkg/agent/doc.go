// Package agent provides the core orchestration loop for GopherAgent.
//
// It contains:
//   - ReAct loop execution (`AgentLoop`)
//   - Streaming/blocking run APIs
//   - Structured error types for `errors.Is`/`errors.As`
//   - Retry configuration for transient LLM failures
//   - Hook and event systems for observability/instrumentation
package agent
