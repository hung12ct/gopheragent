// Package agent provides the core ReAct loop, streaming, and orchestration for LLM agents.
package agent

import (
	"errors"
	"fmt"
	"time"
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

	// ErrHITLTimedOut is returned when AgentLoop.ConfirmHITLTimeout fires
	// before the operator responds. Distinct from ErrHITLDenied so the model
	// can distinguish "user did not respond in time" from "user said no",
	// and so adopters can route on the cause (e.g. retry vs. abort).
	ErrHITLTimedOut = errors.New("agent: human approval timed out")

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

	// ErrConfirmationGateUnconfigured is returned when a tool requires
	// confirmation but no ConfirmHITL callback and no PermissionAllow rule
	// can resolve the gate. The denial is a configuration bug, not a user
	// rejection — the model is told as much so it does not confabulate
	// "the user denied it".
	ErrConfirmationGateUnconfigured = errors.New("agent: tool requires confirmation but no ConfirmHITL handler is configured")

	// ErrNothingToRegenerate is returned by AgentLoop.Regenerate when the
	// session has no user message to replay (system-only history, or every
	// user message was already truncated). The call leaves history and the
	// stream untouched.
	ErrNothingToRegenerate = errors.New("agent: no user message to regenerate")

	// ErrNothingToContinue is returned by AgentLoop.Continue when the
	// session's last persisted message is a clean final-assistant turn —
	// the previous run ended naturally, and the caller should send a new
	// user message instead of trying to extend it.
	ErrNothingToContinue = errors.New("agent: session ended cleanly; nothing to continue")
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

// HITLTimedOutError carries the tool name and configured timeout for an
// expired HITL approval prompt. Surfaced when AgentLoop.ConfirmHITLTimeout
// elapses before the operator responds, so the model directive can ask the
// user to retry instead of routing around the gate.
type HITLTimedOutError struct {
	ToolName string
	Timeout  time.Duration
}

func (e *HITLTimedOutError) Error() string {
	return fmt.Sprintf("agent: human approval for tool %q timed out after %s", e.ToolName, e.Timeout)
}

func (e *HITLTimedOutError) Is(target error) bool {
	return target == ErrHITLTimedOut
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

// ConfirmationGateUnconfiguredError carries the tool name whose
// confirmation gate could not be resolved. Surfaced when a tool declares
// RequiresConfirmation()=true but the AgentLoop has no ConfirmHITL
// callback and no PermissionAllow rule covers the call. This is an
// operator-side configuration bug — the directive in the tool message
// tells the model so explicitly.
type ConfirmationGateUnconfiguredError struct {
	ToolName string
}

func (e *ConfirmationGateUnconfiguredError) Error() string {
	return fmt.Sprintf("agent: tool %q requires confirmation but no ConfirmHITL handler or PermissionAllow rule resolved the gate", e.ToolName)
}

func (e *ConfirmationGateUnconfiguredError) Is(target error) bool {
	return target == ErrConfirmationGateUnconfigured
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
