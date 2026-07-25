package openai

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/sashabaranov/go-openai"
)

func TestClassifyErr(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"nil", nil, false},
		{"unauthorized", &openai.APIError{HTTPStatusCode: 401}, true},
		{"forbidden", &openai.APIError{HTTPStatusCode: 403}, true},
		{"rate limited stays retryable", &openai.APIError{HTTPStatusCode: 429}, false},
		{"server error stays retryable", &openai.APIError{HTTPStatusCode: 500}, false},
		{"plain error untouched", errors.New("connection reset"), false},
		{"wrapped 401 still detected", fmt.Errorf("stream: %w", &openai.APIError{HTTPStatusCode: 401}), true},
		// Compat backends often omit a usable HTTP status; the code is the
		// only signal, so falling back to it avoids misreporting a
		// deterministic auth failure as transient.
		{"compat code without status", &openai.APIError{Code: "invalid_api_key"}, true},
		{"unrelated code without status", &openai.APIError{Code: "context_length_exceeded"}, false},
		{"request error 401", &openai.RequestError{HTTPStatusCode: 401}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyErr(tt.err)
			if tt.err == nil {
				if got != nil {
					t.Fatalf("nil must stay nil, got %v", got)
				}
				return
			}
			if errors.Is(got, agent.ErrLLMAuth) != tt.wantErr {
				t.Fatalf("errors.Is(ErrLLMAuth) = %v, want %v (err=%v)", !tt.wantErr, tt.wantErr, got)
			}
		})
	}
}
