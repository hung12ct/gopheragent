package semconv_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hung12ct/gopheragent/pkg/telemetry/semconv"
)

func TestTruncate(t *testing.T) {
	if got := semconv.Truncate("short", 10); got != "short" {
		t.Fatalf("under limit changed: %q", got)
	}
	if got := semconv.Truncate("anything", 0); got != "anything" {
		t.Fatalf("maxBytes<=0 must disable truncation: %q", got)
	}
	got := semconv.Truncate("abcdefghij", 4)
	if !strings.HasPrefix(got, "abcd…") || !strings.Contains(got, "truncated 6 bytes") {
		t.Fatalf("ascii truncate = %q", got)
	}
	// "é" is 2 bytes; cutting at 3 must back off to a rune boundary.
	got = semconv.Truncate("aéé", 2)
	if !utf8.ValidString(got) || !strings.HasPrefix(got, "a…") {
		t.Fatalf("utf8 truncate = %q", got)
	}
}

func TestCaptureContentFromEnv(t *testing.T) {
	for v, want := range map[string]bool{
		"": false, "false": false, "NO_CONTENT": false, "EVENT_ONLY": false,
		"true": true, "TRUE": true, "SPAN_ONLY": true, " span_and_event ": true,
	} {
		t.Setenv(semconv.EnvCaptureContent, v)
		if got := semconv.CaptureContentFromEnv(); got != want {
			t.Fatalf("%q → %v, want %v", v, got, want)
		}
	}
}

func TestSchemaFromEnv(t *testing.T) {
	t.Setenv(semconv.EnvStabilityOptIn, "http, gen_ai_latest_experimental")
	if semconv.SchemaFromEnv() != semconv.SchemaLatest {
		t.Fatal("comma list with spaces should select latest")
	}
	t.Setenv(semconv.EnvStabilityOptIn, "gen_ai")
	if semconv.SchemaFromEnv() != semconv.SchemaLegacy {
		t.Fatal("partial token must not select latest")
	}
}
