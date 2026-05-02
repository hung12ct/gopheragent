// Package agent provides the core ReAct loop, streaming, and orchestration for LLM agents.
package agent

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by AgentLoop. Use errors.Is / errors.As for matching.
var (
	// ErrMaxIterations is returned when the loop exhausts MaxIters without a final answer.
	ErrMaxIterations = errors.New("agent: reached maximum iterations without final answer")

	// ErrMaxToolCallsPerSession is returned when the cumulative number of
	// tool calls executed across iterations of a single Run exceeds
	// AgentLoop.MaxToolCallsPerSession. Distinct from MaxToolCallsPerTurn,
	// which only caps the wave size within one iteration.
	ErrMaxToolCallsPerSession = errors.New("agent: cumulative tool-call cap exceeded")

	// ErrLoopDetected is returned when the anti-loop detector terminates the cycle.
	ErrLoopDetected = errors.New("agent: infinite loop detected")

	// ErrToolNotFound is returned when the LLM requests a tool that is not registered.
	ErrToolNotFound = errors.New("agent: tool not found")

	// ErrHITLDenied is returned (as a StreamEvent) when a human-in-the-loop operator
	// denies execution of a tool. The loop continues; the LLM is told to find a workaround.
	ErrHITLDenied = errors.New("agent: tool execution denied by human operator")

	// ErrHookRejected is returned when a BeforeHook rejects the request.
	ErrHookRejected = errors.New("agent: request rejected by policy hook")

	// ErrLLMFailure is returned when the LLM provider returns an error during generation.
	ErrLLMFailure = errors.New("agent: LLM generation failed")

	// ErrContextCancelled is returned when the request context is cancelled mid-loop.
	ErrContextCancelled = errors.New("agent: operation cancelled")

	// ErrPermissionDenied is returned (as a StreamEvent) when a configured
	// PermissionChecker rejects a tool call before it runs. Distinct from
	// ErrHITLDenied because no human was in the loop — a policy denied it.
	ErrPermissionDenied = errors.New("agent: tool execution denied by permission policy")
)

// ToolNotFoundError carries the name of the missing tool.
type ToolNotFoundError struct {
	ToolName string
}

func (e *ToolNotFoundError) Error() string {
	return fmt.Sprintf("agent: tool %q not found in registry", e.ToolName)
}

func (e *ToolNotFoundError) Is(target error) bool {
	return target == ErrToolNotFound
}

// HITLDeniedError carries the tool name that was denied.
type HITLDeniedError struct {
	ToolName string
}

func (e *HITLDeniedError) Error() string {
	return fmt.Sprintf("agent: human denied execution of tool %q", e.ToolName)
}

func (e *HITLDeniedError) Is(target error) bool {
	return target == ErrHITLDenied
}

// PermissionDeniedError carries the tool name that was denied by policy.
type PermissionDeniedError struct {
	ToolName string
}

func (e *PermissionDeniedError) Error() string {
	return fmt.Sprintf("agent: permission policy denied execution of tool %q", e.ToolName)
}

func (e *PermissionDeniedError) Is(target error) bool {
	return target == ErrPermissionDenied
}

// HookRejectedError carries the underlying hook error.
type HookRejectedError struct {
	Cause error
}

func (e *HookRejectedError) Error() string {
	return fmt.Sprintf("agent: policy hook rejected request: %v", e.Cause)
}

func (e *HookRejectedError) Is(target error) bool {
	return target == ErrHookRejected
}

func (e *HookRejectedError) Unwrap() error {
	return e.Cause
}

// LLMFailureError carries the underlying provider error.
type LLMFailureError struct {
	Cause error
}

func (e *LLMFailureError) Error() string {
	return fmt.Sprintf("agent: LLM generation failed: %v", e.Cause)
}

func (e *LLMFailureError) Is(target error) bool {
	return target == ErrLLMFailure
}

func (e *LLMFailureError) Unwrap() error {
	return e.Cause
}
