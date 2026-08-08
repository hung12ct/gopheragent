package gemini

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"google.golang.org/genai"
)

// classifyErr tags terminal authentication and configuration failures with
// agent.ErrLLMAuth, leaving every other error untouched.
//
// An invalid key or an unauthorized project fails identically on every
// attempt, so without the tag a retry loop burns a call budget against a
// deterministic config error. Classifying here keeps the SDK dependency
// inside this subpackage: consumers route on the sentinel without linking
// genai.
//
// Returns err unchanged when it is nil or not an auth failure, so call
// sites can wrap unconditionally.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	// 401 is a bad or missing key. 403 covers both a key without access to
	// the model and a Vertex project missing the API enablement, which is
	// the more common misconfiguration of the two.
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("gemini: %w: %w", agent.ErrLLMAuth, err)
	}
	return err
}

// finishReasonErr returns an *agent.IncompleteResponseError unless the
// generation ended cleanly, mapping Gemini's finish reasons onto the
// provider-neutral classification consumers route on. An empty reason
// means the API never reported one (single-chunk responses on some
// endpoints); it is treated as a clean stop so the default path keeps its
// existing behavior exactly.
func finishReasonErr(r genai.FinishReason) error {
	if r == "" || r == genai.FinishReasonStop || r == genai.FinishReasonUnspecified {
		return nil
	}
	return &agent.IncompleteResponseError{
		Provider: "gemini",
		Reason:   string(r),
		Kind:     incompleteKind(r),
	}
}

// incompleteKind splits Gemini's finish reasons into the length cut that
// may pass on a different attempt and the content policy that will not.
// Reasons that are neither (OTHER, MALFORMED_FUNCTION_CALL, …) stay
// unclassified — the response is still partial, but nothing about the
// reason tells a caller whether to retry.
func incompleteKind(r genai.FinishReason) agent.IncompleteKind {
	switch r {
	case genai.FinishReasonMaxTokens:
		return agent.IncompleteTruncated
	case genai.FinishReasonSafety, genai.FinishReasonRecitation,
		genai.FinishReasonBlocklist, genai.FinishReasonProhibitedContent,
		genai.FinishReasonSPII, genai.FinishReasonLanguage,
		genai.FinishReasonImageSafety, genai.FinishReasonImageProhibitedContent,
		genai.FinishReasonImageRecitation:
		return agent.IncompleteBlocked
	}
	return agent.IncompleteOther
}
