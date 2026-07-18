package anthropic

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestApplySampling_StampsTemperatureAndTopP(t *testing.T) {
	p, err := New("test-key", "", WithTemperature(0.2), WithTopP(0.9))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	params := anthropic.MessageNewParams{}
	p.applySampling(&params, false)
	if !params.Temperature.Valid() || params.Temperature.Or(-1) != 0.2 {
		t.Fatalf("expected temperature 0.2, got %+v", params.Temperature)
	}
	if !params.TopP.Valid() || params.TopP.Or(-1) != 0.9 {
		t.Fatalf("expected top_p 0.9, got %+v", params.TopP)
	}
}

func TestApplySampling_ZeroTemperatureIsHonored(t *testing.T) {
	p, err := New("test-key", "", WithTemperature(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	params := anthropic.MessageNewParams{}
	p.applySampling(&params, false)
	if !params.Temperature.Valid() || params.Temperature.Or(-1) != 0 {
		t.Fatalf("explicit 0 must be stamped, got %+v", params.Temperature)
	}
}

func TestApplySampling_UnsetLeavesProviderDefaults(t *testing.T) {
	p, err := New("test-key", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	params := anthropic.MessageNewParams{}
	p.applySampling(&params, false)
	if params.Temperature.Valid() || params.TopP.Valid() {
		t.Fatalf("unset sampling must not stamp fields, got %+v / %+v", params.Temperature, params.TopP)
	}
}

func TestApplySampling_ThinkingSuppressesOverrides(t *testing.T) {
	// The API rejects temperature/top_p alongside extended thinking, so the
	// provider must drop the overrides rather than 400 the call.
	p, err := New("test-key", "", WithTemperature(0.2), WithTopP(0.9))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	params := anthropic.MessageNewParams{}
	p.applySampling(&params, true)
	if params.Temperature.Valid() || params.TopP.Valid() {
		t.Fatalf("thinking-enabled call must not carry sampling overrides, got %+v / %+v", params.Temperature, params.TopP)
	}
}
