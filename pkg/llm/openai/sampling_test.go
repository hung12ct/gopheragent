package openai

import (
	"math"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestApplySampling_StampsAllKnobs(t *testing.T) {
	p, err := New("test-key", "", WithTemperature(0.7), WithTopP(0.9), WithSeed(42))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := openai.ChatCompletionRequest{}
	p.applySampling(&req)
	if req.Temperature != float32(0.7) {
		t.Fatalf("expected temperature 0.7, got %v", req.Temperature)
	}
	if req.TopP != float32(0.9) {
		t.Fatalf("expected top_p 0.9, got %v", req.TopP)
	}
	if req.Seed == nil || *req.Seed != 42 {
		t.Fatalf("expected seed 42, got %v", req.Seed)
	}
}

func TestApplySampling_ZeroTemperatureSurvivesOmitempty(t *testing.T) {
	// The SDK's omitempty drops a literal 0 from the JSON, silently
	// reverting to the provider default — an explicit 0 must be mapped to
	// the smallest positive float32 so it serializes.
	p, err := New("test-key", "", WithTemperature(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := openai.ChatCompletionRequest{}
	p.applySampling(&req)
	if req.Temperature != math.SmallestNonzeroFloat32 {
		t.Fatalf("explicit 0 must map to epsilon, got %v", req.Temperature)
	}
}

func TestApplySampling_UnsetLeavesProviderDefaults(t *testing.T) {
	p, err := New("test-key", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := openai.ChatCompletionRequest{}
	p.applySampling(&req)
	if req.Temperature != 0 || req.TopP != 0 || req.Seed != nil {
		t.Fatalf("unset sampling must not touch req, got %+v", req)
	}
}

func TestNewCompat_AppliesOptions(t *testing.T) {
	p, err := NewCompat("test-key", "llama3", "http://localhost:11434/v1", WithSeed(7))
	if err != nil {
		t.Fatalf("NewCompat: %v", err)
	}
	req := openai.ChatCompletionRequest{}
	p.applySampling(&req)
	if req.Seed == nil || *req.Seed != 7 {
		t.Fatalf("NewCompat must apply options, got %v", req.Seed)
	}
}
