package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
)

// sse renders one Messages-API stream event.
func sse(event, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, data)
}

// textStream builds the event sequence for a single text block that ends
// with the given stop reason.
func textStream(text, stopReason string) []string {
	return []string{
		sse("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`),
		sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sse("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text)),
		sse("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sse("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":8}}`, stopReason)),
		sse("message_stop", `{"type":"message_stop"}`),
	}
}

// streamProvider returns a Provider whose Messages endpoint replays the
// given raw SSE frames, so the accumulation loop can be exercised without
// a live backend.
func streamProvider(t *testing.T, frames ...string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprint(w, f)
		}
	}))
	t.Cleanup(srv.Close)

	client := anthropic.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(srv.URL+"/"),
	)
	return &Provider{client: &client, model: anthropic.Model("claude-test"), MaxTokens: 1024}
}

// runStream drives one GenerateStream call and returns the result, every
// event the provider emitted, and the call error.
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
	// The payload is a valid JSON prefix — the shape that reaches a caller
	// as "unexpected end of JSON input" when the cap goes unreported.
	p := streamProvider(t, textStream(`{"name":"ab`, "max_tokens")...)

	res, events, err := runStream(t, p)
	if !errors.Is(err, agent.ErrLLMTruncated) {
		t.Fatalf("errors.Is(ErrLLMTruncated) = false, got %v", err)
	}
	if errors.Is(err, agent.ErrLLMContentBlocked) {
		t.Fatalf("a length cut must not classify as a content block: %v", err)
	}
	var inc *agent.IncompleteResponseError
	if !errors.As(err, &inc) || inc.Provider != "anthropic" || inc.Reason != "max_tokens" {
		t.Fatalf("want *agent.IncompleteResponseError{anthropic, max_tokens}, got %#v", err)
	}
	// The partial rides on the result so a host can show what arrived.
	if res.Content != `{"name":"ab` {
		t.Fatalf("partial content: got %q", res.Content)
	}

	var limits int
	for _, ev := range events {
		if l, ok := ev.Payload.(agent.LimitExhaustedEvent); ok {
			limits++
			if l.Kind != agent.LimitKindProviderMaxTokens || l.Limit != 1024 {
				t.Fatalf("limit event: got %+v", l)
			}
		}
	}
	if limits != 1 {
		t.Fatalf("want exactly 1 LimitExhaustedEvent, got %d", limits)
	}
}

func TestGenerateStream_RefusalIsBlocked(t *testing.T) {
	p := streamProvider(t, textStream("I can't", "refusal")...)

	_, _, err := runStream(t, p)
	if !errors.Is(err, agent.ErrLLMContentBlocked) {
		t.Fatalf("errors.Is(ErrLLMContentBlocked) = false, got %v", err)
	}
	if errors.Is(err, agent.ErrLLMTruncated) {
		t.Fatalf("a refusal must not classify as truncation: %v", err)
	}
}

func TestGenerateStream_CleanStopUnchanged(t *testing.T) {
	p := streamProvider(t, textStream("hello world", "end_turn")...)

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

func TestStopReasonErr_Classification(t *testing.T) {
	tests := []struct {
		reason  anthropic.StopReason
		wantErr bool
		target  error // nil = matches neither sentinel
	}{
		{"", false, nil},
		{anthropic.StopReasonEndTurn, false, nil},
		{anthropic.StopReasonStopSequence, false, nil},
		{anthropic.StopReasonToolUse, false, nil},
		// pause_turn is a complete response the caller is asked to continue,
		// not a cut one.
		{anthropic.StopReasonPauseTurn, false, nil},
		{anthropic.StopReasonMaxTokens, true, agent.ErrLLMTruncated},
		{anthropic.StopReasonRefusal, true, agent.ErrLLMContentBlocked},
		{"model_context_window_exceeded", true, nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.reason), func(t *testing.T) {
			err := stopReasonErr(tt.reason)
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
