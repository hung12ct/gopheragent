package anthropic

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/hung12ct/gopheragent/pkg/agent"
)

// classifyErr tags terminal authentication and configuration failures with
// agent.ErrLLMAuth, leaving every other error untouched.
//
// The distinction matters to callers, not to us: an invalid key fails the
// same way on every attempt, so a retry loop burns a call budget for nothing
// and reports a config problem as a flaky backend. Classifying here rather
// than in pkg/agent is what keeps the SDK dependency inside this
// subpackage — the consumer routes on the sentinel without linking
// anthropic's SDK.
//
// Returns err unchanged when it is nil or not an auth failure, so call
// sites can wrap unconditionally.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.StatusCode {
	// 401 is a bad or missing key; 403 is a key that authenticates but is
	// not permitted this model or project. Both are deterministic, so both
	// belong on the do-not-retry side.
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("anthropic: %w: %w", agent.ErrLLMAuth, err)
	}
	return err
}

// stopReasonErr returns an *agent.IncompleteResponseError unless the
// generation ended cleanly, mapping Anthropic's stop reasons onto the
// provider-neutral classification consumers route on.
//
// "end_turn", "stop_sequence", and "tool_use" are complete responses.
// "pause_turn" is also complete — a long-running server tool asked the
// caller to continue the turn, and the content so far is intact. An empty
// reason means the API never reported one, treated as a clean stop so the
// default path is unchanged.
func stopReasonErr(r anthropic.StopReason) error {
	var kind agent.IncompleteKind
	switch r {
	case "", anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence,
		anthropic.StopReasonToolUse, anthropic.StopReasonPauseTurn:
		return nil
	case anthropic.StopReasonMaxTokens:
		kind = agent.IncompleteTruncated
	case anthropic.StopReasonRefusal:
		kind = agent.IncompleteBlocked
	default:
		kind = agent.IncompleteOther
	}
	return &agent.IncompleteResponseError{Provider: "anthropic", Reason: string(r), Kind: kind}
}
