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

	// ErrDegraded classifies a turn that produced a real answer while some
	// tool's derived bookkeeping failed — neither a clean success nor a
	// failure worth retrying. It is never returned from Run (the answer is
	// good); adopters match it via errors.Is on DegradedEvent.Err.
	ErrDegraded = errors.New("agent: turn completed with degraded state")

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

	// ErrLLMAuth is returned when the provider rejects the request for
	// authentication or configuration reasons — a missing, invalid, or
	// revoked API key, or a project without access to the model.
	//
	// It is deliberately distinct from ErrLLMFailure because the two demand
	// opposite responses. A generation failure is transient and retrying is
	// reasonable; an auth failure is deterministic, so every retry fails
	// identically. Without the distinction a misconfigured key looks exactly
	// like a flaky backend: retry loops spin, per-run call and spend budgets
	// drain against attempts that never had a chance, and the work gets
	// parked as bad input rather than surfaced as "fix your key".
	//
	// Providers classify their own terminal auth responses, so route on the
	// sentinel rather than matching provider message text:
	//
	//	if errors.Is(err, agent.ErrLLMAuth) {
	//	    return fmt.Errorf("check the provider credentials: %w", err)
	//	}
	ErrLLMAuth = errors.New("agent: LLM provider authentication or configuration failed")

	// ErrLLMTruncated is returned when the provider stopped generating
	// before the response was finished — an output-token cap, typically.
	//
	// The danger is that a truncated response is a *valid prefix*: the text
	// looks fine and only fails when something downstream parses it, so the
	// operator reads "unexpected end of JSON input" and inspects a schema
	// that was never wrong. Providers detect the cut at the source, so route
	// on the sentinel instead of matching decode-error text.
	//
	// The cut is a function of this response's length rather than of the
	// request, so isRetryable leaves it retryable — but a truncation that
	// already streamed content is not retried, because re-running it would
	// replay the same text into the consumer's stream. The real fix is a
	// larger provider token cap or a smaller ask, both caller decisions.
	ErrLLMTruncated = errors.New("agent: LLM response truncated before completion")

	// ErrLLMContentBlocked is returned when the provider stopped generating
	// for a content policy: safety, recitation, a blocklist, or an
	// unsupported language.
	//
	// Deliberately distinct from ErrLLMTruncated because the two demand
	// opposite responses. A truncation is length-dependent and may pass on
	// the next attempt; a policy stop is deterministic for a given prompt,
	// so every retry reproduces it. isRetryable treats it as terminal — the
	// caller must change the request or surface the block to the user.
	ErrLLMContentBlocked = errors.New("agent: LLM stopped generating for a content policy")

	// ErrUnrenderablePart is returned by a provider adapter when a message
	// carries a history.MediaPart the adapter cannot put on the wire — an
	// unsupported part type, or a part whose payload is missing.
	//
	// Failing is the point. The alternative, dropping the part and sending
	// the rest, produces a fluent well-formed answer from a model that never
	// received the media, and nothing distinguishes that from success: not
	// the response, not the logs, not a schema check. A judge that cannot see
	// the image it is judging is not a degraded judge, it is a random one.
	//
	// Deterministic like ErrLLMAuth, not transient like ErrLLMFailure — the
	// same message fails identically on every retry. isRetryable treats it as
	// terminal; the caller must re-encode the part or choose a provider that
	// supports it.
	ErrUnrenderablePart = errors.New("agent: message carries a media part this provider cannot render")

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

// IncompleteKind classifies why a provider stopped generating early. Every
// provider has its own stop-reason vocabulary ("MAX_TOKENS", "length",
// "max_tokens"); each translates into these three cases so consumers route
// on one contract instead of three.
type IncompleteKind string

const (
	// IncompleteTruncated: an output-length cap cut the response short.
	IncompleteTruncated IncompleteKind = "truncated"
	// IncompleteBlocked: a content policy stopped the generation.
	IncompleteBlocked IncompleteKind = "blocked"
	// IncompleteOther: neither a length cap nor a policy — a malformed
	// tool call, or a backend stop the provider does not classify. The
	// response is still partial, but it matches no sentinel, so retry
	// behavior stays at the default.
	IncompleteOther IncompleteKind = "other"
)

// IncompleteResponseError reports a generation that ended before the model
// finished. Every provider returns it for the same situations — an output
// cap fired, or a content filter stopped the stream — so an adopter writes
// one branch rather than one per vendor:
//
//	if errors.Is(err, agent.ErrLLMTruncated) {
//	    // raise the provider's token cap, or ask for less
//	}
//
// Whatever streamed before the stop still rides on the returned LLMResult
// (content and usage), because it is real output that cost real tokens —
// it is a prefix, though, never a complete answer. Reason carries the raw
// provider stop reason for logs; Kind is what routing should key on, via
// the sentinels above.
type IncompleteResponseError struct {
	// Provider names the vendor package that produced the error, matching
	// the error-message prefix convention ("anthropic", "openai", "gemini").
	Provider string
	// Reason is the provider's own stop reason, verbatim.
	Reason string
	// Kind is the provider-neutral classification.
	Kind IncompleteKind
}

func (e *IncompleteResponseError) Error() string {
	return fmt.Sprintf("%s: generation stopped early (%s): the response is a partial prefix, not a complete answer", e.Provider, e.Reason)
}

func (e *IncompleteResponseError) Is(target error) bool {
	switch e.Kind {
	case IncompleteTruncated:
		return target == ErrLLMTruncated
	case IncompleteBlocked:
		return target == ErrLLMContentBlocked
	}
	return false
}
