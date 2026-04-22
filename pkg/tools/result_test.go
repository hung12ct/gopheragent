package tools

import "testing"

func TestNoopEmitter_DropsPayload(t *testing.T) {
	emit := NoopEmitter()
	if err := emit.Partial("anything"); err != nil {
		t.Fatalf("noop emitter should never error, got %v", err)
	}
	if err := emit.Partial(map[string]any{"k": 1}); err != nil {
		t.Fatalf("noop emitter should never error, got %v", err)
	}
}

func TestNoopEmitter_ReturnsSameSingleton(t *testing.T) {
	a, b := NoopEmitter(), NoopEmitter()
	if a != b {
		t.Fatal("NoopEmitter() must return the shared singleton — repeated calls must not allocate a fresh instance")
	}
}
