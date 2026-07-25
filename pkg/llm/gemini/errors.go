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
