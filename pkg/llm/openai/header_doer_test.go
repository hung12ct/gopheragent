package openai

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// recordingDoer captures the request headerDoer hands to the underlying client.
type recordingDoer struct{ got *http.Request }

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	*d.got = *req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

// The doer must not write through to the caller's request: go-openai reuses
// the request it built, and a mutated header would leak the injected values
// into unrelated call sites.
func TestHeaderDoerLeavesCallerRequestUntouched(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/chat/completions", http.NoBody)
	req.Header.Set("Authorization", "Bearer original")

	var seen http.Request
	d := headerDoer{
		base:    &recordingDoer{got: &seen},
		headers: map[string]string{"X-Title": "gopheragent"},
	}
	if _, err := d.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}

	if got := req.Header.Get("X-Title"); got != "" {
		t.Fatalf("caller request X-Title = %q, want it left unset", got)
	}
	if got := seen.Header.Get("X-Title"); got != "gopheragent" {
		t.Fatalf("forwarded X-Title = %q, want gopheragent", got)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer original" {
		t.Fatalf("forwarded Authorization = %q, want the SDK value preserved", got)
	}
}

// Headers are applied last, so a caller can deliberately replace one the SDK
// set. WithHTTPHeader documents this; the test pins it as intended behavior.
func TestHeaderDoerOverridesSDKHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/chat/completions", http.NoBody)
	req.Header.Set("Authorization", "Bearer original")

	var seen http.Request
	d := headerDoer{
		base:    &recordingDoer{got: &seen},
		headers: map[string]string{"Authorization": "Bearer override"},
	}
	if _, err := d.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer override" {
		t.Fatalf("forwarded Authorization = %q, want Bearer override", got)
	}
}
