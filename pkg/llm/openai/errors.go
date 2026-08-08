package openai

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/sashabaranov/go-openai"
)

// classifyErr tags terminal authentication and configuration failures with
// agent.ErrLLMAuth, leaving every other error untouched.
//
// An invalid key fails identically on every attempt, so without the tag a
// retry loop burns a call budget against a deterministic config error and
// reports it as a flaky backend. Classifying here keeps the SDK dependency
// inside this subpackage: consumers route on the sentinel without linking
// go-openai.
//
// Returns err unchanged when it is nil or not an auth failure, so call
// sites can wrap unconditionally.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		// 401 is a bad or missing key; 403 is a key that authenticates but
		// lacks access to the model or project. Both are deterministic.
		switch apiErr.HTTPStatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("openai: %w: %w", agent.ErrLLMAuth, err)
		}
		// Compat endpoints (Ollama, Groq, vLLM) frequently return the code
		// without a usable HTTP status, so fall back to it rather than
		// misclassifying a genuine auth failure as transient.
		if code, ok := apiErr.Code.(string); ok {
			switch code {
			case "invalid_api_key", "account_deactivated":
				return fmt.Errorf("openai: %w: %w", agent.ErrLLMAuth, err)
			}
		}
		return err
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		switch reqErr.HTTPStatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("openai: %w: %w", agent.ErrLLMAuth, err)
		}
	}
	return err
}

// finishReasonErr returns an *agent.IncompleteResponseError unless the
// generation ended cleanly, mapping OpenAI's finish reasons onto the
// provider-neutral classification consumers route on.
//
// "stop", "tool_calls", and "function_call" are complete responses; an
// empty or "null" reason means the API never reported one, which is
// treated as a clean stop so the default path is unchanged.
func finishReasonErr(r openai.FinishReason) error {
	var kind agent.IncompleteKind
	switch r {
	case "", openai.FinishReasonNull, openai.FinishReasonStop,
		openai.FinishReasonToolCalls, openai.FinishReasonFunctionCall:
		return nil
	case openai.FinishReasonLength:
		kind = agent.IncompleteTruncated
	case openai.FinishReasonContentFilter:
		kind = agent.IncompleteBlocked
	default:
		// Compat backends (Ollama, Groq, vLLM) invent their own reasons.
		// Unknown means the response is not a documented clean stop, so
		// report it as partial rather than assume it finished.
		kind = agent.IncompleteOther
	}
	return &agent.IncompleteResponseError{Provider: "openai", Reason: string(r), Kind: kind}
}
