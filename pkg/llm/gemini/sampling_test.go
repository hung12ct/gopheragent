package gemini

import (
	"testing"

	"google.golang.org/genai"
)

func TestApplySampling_StampsAllKnobs(t *testing.T) {
	p, err := New("test-key", "", WithTemperature(0.7), WithTopP(0.9), WithSeed(42))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	config := &genai.GenerateContentConfig{}
	p.applySampling(config)
	if config.Temperature == nil || *config.Temperature != float32(0.7) {
		t.Fatalf("expected temperature 0.7, got %v", config.Temperature)
	}
	if config.TopP == nil || *config.TopP != float32(0.9) {
		t.Fatalf("expected top_p 0.9, got %v", config.TopP)
	}
	if config.Seed == nil || *config.Seed != 42 {
		t.Fatalf("expected seed 42, got %v", config.Seed)
	}
}

func TestApplySampling_ZeroTemperatureIsHonored(t *testing.T) {
	p, err := New("test-key", "", WithTemperature(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	config := &genai.GenerateContentConfig{}
	p.applySampling(config)
	if config.Temperature == nil || *config.Temperature != 0 {
		t.Fatalf("explicit 0 must be stamped, got %v", config.Temperature)
	}
}

func TestApplySampling_UnsetLeavesProviderDefaults(t *testing.T) {
	p, err := New("test-key", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	config := &genai.GenerateContentConfig{}
	p.applySampling(config)
	if config.Temperature != nil || config.TopP != nil || config.Seed != nil {
		t.Fatalf("unset sampling must not touch config, got %+v", config)
	}
}
