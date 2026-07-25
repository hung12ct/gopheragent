package gemini

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"google.golang.org/genai"
)

func TestClassifyErr(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"nil", nil, false},
		{"unauthorized", genai.APIError{Code: 401, Message: "API key not valid"}, true},
		{"forbidden covers vertex api not enabled", genai.APIError{Code: 403}, true},
		{"rate limited stays retryable", genai.APIError{Code: 429}, false},
		{"server error stays retryable", genai.APIError{Code: 500}, false},
		{"plain error untouched", errors.New("connection reset"), false},
		{"wrapped 401 still detected", fmt.Errorf("stream: %w", genai.APIError{Code: 401}), true},
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
