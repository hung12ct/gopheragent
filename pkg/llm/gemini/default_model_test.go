package gemini

import "testing"

// A retired default fails every fresh signup on its first call while old keys
// keep passing, so pin the value and check every constructor honors it.
func TestConstructorsUseDefaultModel(t *testing.T) {
	if defaultModel != "gemini-3.8-flash" {
		t.Fatalf("defaultModel = %q, want gemini-3.8-flash", defaultModel)
	}
	p, err := New("test-key", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tr, err := NewTranscriber("test-key", "")
	if err != nil {
		t.Fatalf("NewTranscriber: %v", err)
	}
	ma, err := NewMediaAnalyzer("test-key", "")
	if err != nil {
		t.Fatalf("NewMediaAnalyzer: %v", err)
	}
	for name, got := range map[string]string{"New": p.model, "NewTranscriber": tr.model, "NewMediaAnalyzer": ma.model} {
		if got != defaultModel {
			t.Fatalf("%s model = %q, want %q", name, got, defaultModel)
		}
	}
}
