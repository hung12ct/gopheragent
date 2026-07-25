package anthropic

import (
	"errors"
	"fmt"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/hung12ct/gopheragent/pkg/agent"
)

func TestClassifyErr(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr bool // expect agent.ErrLLMAuth in the chain
	}{
		{"nil", nil, false},
		{"unauthorized", &anthropic.Error{StatusCode: 401}, true},
		{"forbidden", &anthropic.Error{StatusCode: 403}, true},
		{"rate limited stays retryable", &anthropic.Error{StatusCode: 429}, false},
		{"server error stays retryable", &anthropic.Error{StatusCode: 500}, false},
		{"bad request is not auth", &anthropic.Error{StatusCode: 400}, false},
		{"plain error untouched", errors.New("connection reset"), false},
		{"wrapped 401 still detected", fmt.Errorf("stream: %w", &anthropic.Error{StatusCode: 401}), true},
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
			// The original error must stay reachable either way.
			if !errors.Is(got, tt.err) && got != tt.err {
				t.Fatalf("original error lost: %v", got)
			}
		})
	}
}
