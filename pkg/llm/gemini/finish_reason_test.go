package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"google.golang.org/genai"
)

// streamProvider returns a Provider whose streaming endpoint replays the
// given JSON chunks as server-sent events, so the accumulation loop can be
// exercised without a live Gemini backend.
func streamProvider(t *testing.T, chunks ...string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\r\n\r\n", c)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:      "test-key",
		Backend:     genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL},
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return &Provider{client: client, model: "gemini-test"}
}

// runStream drives one GenerateStream call and returns the result, the
// error, and every event the provider emitted.
func runStream(t *testing.T, p *Provider) (agent.LLMResult, []agent.StreamEvent, error) {
	t.Helper()
	ch := make(chan agent.StreamEvent, 64)
	res, err := p.GenerateStream(context.Background(), []history.Message{{Role: "user", Content: "hi"}}, nil, ch)
	close(ch)
	var events []agent.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return res, events, err
}

func TestGenerateStream_MaxTokensSurfacesTruncation(t *testing.T) {
	// The payload is a valid JSON prefix — exactly the shape that used to
	// reach the caller as a success and fail at json.Unmarshal upstream.
	p := streamProvider(t,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"name\":\"ab"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`,
		`{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":8,"totalTokenCount":18}}`,
	)

	res, events, err := runStream(t, p)
	if err == nil {
		t.Fatal("truncated stream must not return a nil error")
	}
	if !errors.Is(err, agent.ErrLLMTruncated) {
		t.Fatalf("errors.Is(ErrLLMTruncated) = false, got %v", err)
	}
	if errors.Is(err, agent.ErrLLMContentBlocked) {
		t.Fatalf("a length cut must not classify as a content block: %v", err)
	}
	var inc *agent.IncompleteResponseError
	if !errors.As(err, &inc) || inc.Reason != "MAX_TOKENS" || inc.Provider != "gemini" {
		t.Fatalf("want *agent.IncompleteResponseError{gemini, MAX_TOKENS}, got %#v", err)
	}
	// The partial rides on the result so adopters can show what arrived.
	if res.Content != `{"name":"ab` {
		t.Fatalf("partial content: got %q", res.Content)
	}
	if res.Usage.TotalTokens != 18 {
		t.Fatalf("usage must survive the error path: got %+v", res.Usage)
	}

	var limits int
	for _, ev := range events {
		if l, ok := ev.Payload.(agent.LimitExhaustedEvent); ok {
			limits++
			if l.Kind != agent.LimitKindProviderMaxTokens {
				t.Fatalf("limit kind: got %q", l.Kind)
			}
			if l.Used != 8 {
				t.Fatalf("limit used: want 8, got %d", l.Used)
			}
		}
	}
	if limits != 1 {
		t.Fatalf("want exactly 1 LimitExhaustedEvent, got %d", limits)
	}
}

func TestGenerateStream_SafetyStopWithoutContent(t *testing.T) {
	// A content filter blanks the candidate's Content, so the reason must be
	// read before the nil-Content guard skips the chunk.
	p := streamProvider(t,
		`{"candidates":[{"finishReason":"SAFETY"}]}`,
	)

	_, _, err := runStream(t, p)
	if !errors.Is(err, agent.ErrLLMContentBlocked) {
		t.Fatalf("errors.Is(ErrLLMContentBlocked) = false, got %v", err)
	}
	if errors.Is(err, agent.ErrLLMTruncated) {
		t.Fatalf("a policy stop must not classify as truncation: %v", err)
	}
}

func TestGenerateStream_CleanStopUnchanged(t *testing.T) {
	p := streamProvider(t,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"hello "}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"world"}]},"finishReason":"STOP"}]}`,
	)

	res, events, err := runStream(t, p)
	if err != nil {
		t.Fatalf("clean stop must not error: %v", err)
	}
	if res.Content != "hello world" {
		t.Fatalf("content: got %q", res.Content)
	}
	for _, ev := range events {
		if _, ok := ev.Payload.(agent.LimitExhaustedEvent); ok {
			t.Fatal("clean stop must not emit a limit event")
		}
	}
}

func TestFinishReasonErr_Classification(t *testing.T) {
	tests := []struct {
		reason  genai.FinishReason
		wantErr bool
		target  error // nil = matches neither sentinel
	}{
		{"", false, nil},
		{genai.FinishReasonStop, false, nil},
		{genai.FinishReasonUnspecified, false, nil},
		{genai.FinishReasonMaxTokens, true, agent.ErrLLMTruncated},
		{genai.FinishReasonSafety, true, agent.ErrLLMContentBlocked},
		{genai.FinishReasonRecitation, true, agent.ErrLLMContentBlocked},
		{genai.FinishReasonBlocklist, true, agent.ErrLLMContentBlocked},
		{genai.FinishReasonProhibitedContent, true, agent.ErrLLMContentBlocked},
		{genai.FinishReasonSPII, true, agent.ErrLLMContentBlocked},
		{genai.FinishReasonLanguage, true, agent.ErrLLMContentBlocked},
		{genai.FinishReasonImageSafety, true, agent.ErrLLMContentBlocked},
		{genai.FinishReasonMalformedFunctionCall, true, nil},
		{genai.FinishReasonOther, true, nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.reason), func(t *testing.T) {
			err := finishReasonErr(tt.reason)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil {
				return
			}
			for _, sentinel := range []error{agent.ErrLLMTruncated, agent.ErrLLMContentBlocked} {
				want := sentinel == tt.target
				if errors.Is(err, sentinel) != want {
					t.Fatalf("errors.Is(%v) = %v, want %v", sentinel, !want, want)
				}
			}
		})
	}
}
