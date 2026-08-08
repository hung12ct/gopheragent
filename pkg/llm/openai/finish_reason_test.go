package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/sashabaranov/go-openai"
)

// streamProvider returns a Provider whose chat-completions endpoint
// replays the given JSON chunks as server-sent events, so the accumulation
// loop can be exercised without a live backend.
func streamProvider(t *testing.T, chunks ...string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	p, err := NewCompat("test-key", "gpt-test", srv.URL+"/v1")
	if err != nil {
		t.Fatalf("NewCompat: %v", err)
	}
	return p
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

func TestGenerateStream_LengthSurfacesTruncation(t *testing.T) {
	// The payload is a valid JSON prefix — the shape that used to reach the
	// caller as a success and fail at json.Unmarshal upstream.
	p := streamProvider(t,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"{\"name\":\"ab"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
	)

	res, events, err := runStream(t, p)
	if !errors.Is(err, agent.ErrLLMTruncated) {
		t.Fatalf("errors.Is(ErrLLMTruncated) = false, got %v", err)
	}
	if errors.Is(err, agent.ErrLLMContentBlocked) {
		t.Fatalf("a length cut must not classify as a content block: %v", err)
	}
	var inc *agent.IncompleteResponseError
	if !errors.As(err, &inc) || inc.Provider != "openai" || inc.Reason != "length" {
		t.Fatalf("want *agent.IncompleteResponseError{openai, length}, got %#v", err)
	}
	if res.Content != `{"name":"ab` {
		t.Fatalf("partial content: got %q", res.Content)
	}

	var limits int
	for _, ev := range events {
		if l, ok := ev.Payload.(agent.LimitExhaustedEvent); ok {
			limits++
			if l.Kind != agent.LimitKindProviderMaxTokens {
				t.Fatalf("limit kind: got %q", l.Kind)
			}
		}
	}
	if limits != 1 {
		t.Fatalf("want exactly 1 LimitExhaustedEvent, got %d", limits)
	}
}

func TestGenerateStream_ContentFilterIsBlocked(t *testing.T) {
	// A filtered response carries an empty delta, so the reason must be read
	// before the delta is consumed.
	p := streamProvider(t,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`,
	)

	_, events, err := runStream(t, p)
	if !errors.Is(err, agent.ErrLLMContentBlocked) {
		t.Fatalf("errors.Is(ErrLLMContentBlocked) = false, got %v", err)
	}
	if errors.Is(err, agent.ErrLLMTruncated) {
		t.Fatalf("a policy stop must not classify as truncation: %v", err)
	}
	for _, ev := range events {
		if _, ok := ev.Payload.(agent.LimitExhaustedEvent); ok {
			t.Fatal("a content filter is not a token cap — no limit event")
		}
	}
}

func TestGenerateStream_CleanStopUnchanged(t *testing.T) {
	p := streamProvider(t,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello "}}]}`,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}]}`,
	)

	res, _, err := runStream(t, p)
	if err != nil {
		t.Fatalf("clean stop must not error: %v", err)
	}
	if res.Content != "hello world" {
		t.Fatalf("content: got %q", res.Content)
	}
}

func TestGenerateStream_ToolCallsAreACleanStop(t *testing.T) {
	// tool_calls terminates a complete response; erroring here would break
	// every ReAct turn that dispatches a tool.
	p := streamProvider(t,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search","arguments":"{}"}}]}}]}`,
		`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	res, _, err := runStream(t, p)
	if err != nil {
		t.Fatalf("tool_calls must not error: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "search" {
		t.Fatalf("tool calls: got %+v", res.ToolCalls)
	}
}

func TestFinishReasonErr_Classification(t *testing.T) {
	tests := []struct {
		reason  openai.FinishReason
		wantErr bool
		target  error // nil = matches neither sentinel
	}{
		{"", false, nil},
		{openai.FinishReasonNull, false, nil},
		{openai.FinishReasonStop, false, nil},
		{openai.FinishReasonToolCalls, false, nil},
		{openai.FinishReasonFunctionCall, false, nil},
		{openai.FinishReasonLength, true, agent.ErrLLMTruncated},
		{openai.FinishReasonContentFilter, true, agent.ErrLLMContentBlocked},
		// Compat backends invent reasons; unknown is reported as partial.
		{"eos", true, nil},
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
