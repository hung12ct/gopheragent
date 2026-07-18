package openai

import (
	"context"
	"errors"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

func TestNewSummaryProvider_NoKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	_, err := NewSummaryProvider("", "")
	if err == nil {
		t.Fatal("expected error when no API key")
	}
	if !errors.Is(err, err) || err.Error() == "" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewSummaryProvider_WithKey(t *testing.T) {
	sp, err := NewSummaryProvider("sk-test-key", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sp == nil {
		t.Fatal("expected non-nil provider")
	}
}

func TestSummaryProvider_EmptyHistory(t *testing.T) {
	sp, _ := NewSummaryProvider("sk-test-key", "gpt-4o-mini")

	// Empty history should return empty string without calling API
	result, err := sp.SummarizeBehaviors(context.Background(), []history.Message{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result for empty history, got %q", result)
	}
}

func TestSummaryProvider_SystemOnlyHistory(t *testing.T) {
	sp, _ := NewSummaryProvider("sk-test-key", "gpt-4o-mini")

	msgs := []history.Message{
		{Role: "system", Content: "You are an assistant."},
	}
	// system-only: no real content to summarize — should return empty without calling API
	result, err := sp.SummarizeBehaviors(context.Background(), msgs, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty for system-only history, got %q", result)
	}
}
