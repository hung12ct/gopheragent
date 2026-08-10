package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/audio"
)

// transcriptionServer captures the multipart body the client sends and
// replies with a fixed verbose_json payload.
func transcriptionServer(t *testing.T, body string) (*httptest.Server, *string) {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func newTestTranscriber(t *testing.T, srv *httptest.Server, model string) *Transcriber {
	t.Helper()
	tr, err := NewTranscriber("test-key", model, WithBaseURL(srv.URL+"/v1"))
	if err != nil {
		t.Fatalf("NewTranscriber: %v", err)
	}
	return tr
}

func TestTranscribeMapsSegmentsAndDuration(t *testing.T) {
	const resp = `{"task":"transcribe","language":"english","duration":3.5,
		"segments":[{"id":0,"start":0.0,"end":1.25,"text":" Hello there"},
		            {"id":1,"start":1.25,"end":3.5,"text":" second part "}],
		"text":"  Hello there second part  "}`
	srv, _ := transcriptionServer(t, resp)

	out, err := newTestTranscriber(t, srv, "").
		Transcribe(context.Background(), audio.Clip{MIME: "audio/wav", Data: []byte("RIFF")}, audio.Options{})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if out.Text != "Hello there second part" {
		t.Fatalf("Text = %q, want trimmed transcript", out.Text)
	}
	if out.Language != "english" {
		t.Fatalf("Language = %q, want english", out.Language)
	}
	// 3.5s must survive as sub-second precision, not truncate to 3s.
	if out.Duration != 3500*time.Millisecond {
		t.Fatalf("Duration = %v, want 3.5s", out.Duration)
	}
	if len(out.Segments) != 2 {
		t.Fatalf("Segments = %d, want 2", len(out.Segments))
	}
	if out.Segments[0].End != 1250*time.Millisecond || out.Segments[0].Text != "Hello there" {
		t.Fatalf("Segments[0] = %+v, want end 1.25s and trimmed text", out.Segments[0])
	}
}

// The filename extension is the only signal the endpoint has for the
// container format, so a wrong one rejects a clip that would decode. A codec
// parameter in the MIME type must not leak into it.
func TestTranscribeSendsExtensionDerivedFromMIME(t *testing.T) {
	srv, body := transcriptionServer(t, `{"text":"ok"}`)
	tr := newTestTranscriber(t, srv, "")

	clip := audio.Clip{MIME: "audio/webm;codecs=opus", Data: []byte("webm-bytes")}
	if _, err := tr.Transcribe(context.Background(), clip, audio.Options{}); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if !strings.Contains(*body, `filename="clip.webm"`) {
		t.Fatalf("multipart body missing clip.webm filename:\n%s", *body)
	}
}

func TestTranscribeForwardsLanguageAndPrompt(t *testing.T) {
	srv, body := transcriptionServer(t, `{"text":"ok"}`)
	tr := newTestTranscriber(t, srv, "")

	opts := audio.Options{Language: "vi", Prompt: "GopherAgent, Parakeet"}
	clip := audio.Clip{MIME: "audio/wav", Data: []byte("RIFF")}
	if _, err := tr.Transcribe(context.Background(), clip, opts); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	for _, want := range []string{"vi", "GopherAgent, Parakeet"} {
		if !strings.Contains(*body, want) {
			t.Fatalf("multipart body missing %q:\n%s", want, *body)
		}
	}
}

// gpt-4o-transcribe rejects verbose_json outright, so asking for it would
// fail every request rather than degrading to a transcript without timings.
func TestTranscribeOmitsVerboseFormatForNonWhisperModels(t *testing.T) {
	for _, tc := range []struct{ model, wantFormat string }{
		{"whisper-1", "verbose_json"},
		{"gpt-4o-transcribe", "json"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			srv, body := transcriptionServer(t, `{"text":"ok"}`)
			tr := newTestTranscriber(t, srv, tc.model)
			clip := audio.Clip{MIME: "audio/wav", Data: []byte("RIFF")}
			if _, err := tr.Transcribe(context.Background(), clip, audio.Options{}); err != nil {
				t.Fatalf("Transcribe: %v", err)
			}
			if !strings.Contains(*body, tc.wantFormat) {
				t.Fatalf("model %s: body missing response_format %s:\n%s", tc.model, tc.wantFormat, *body)
			}
			if tc.wantFormat == "json" && strings.Contains(*body, "verbose_json") {
				t.Fatalf("model %s: body requested verbose_json:\n%s", tc.model, *body)
			}
		})
	}
}

// An oversized clip must fail before the upload, and as ErrTooLarge rather
// than a generic error, so a capture pipeline knows to cut shorter chunks.
func TestTranscribeRejectsOversizedClipWithoutCallingAPI(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	clip := audio.Clip{MIME: "audio/wav", Data: make([]byte, maxClipBytes+1)}
	_, err := newTestTranscriber(t, srv, "").Transcribe(context.Background(), clip, audio.Options{})
	if !errors.Is(err, audio.ErrTooLarge) {
		t.Fatalf("Transcribe error = %v, want ErrTooLarge", err)
	}
	if called {
		t.Fatal("oversized clip was uploaded; want a local rejection")
	}
}

func TestTranscribeRejectsInvalidClip(t *testing.T) {
	srv, _ := transcriptionServer(t, `{"text":"ok"}`)
	tr := newTestTranscriber(t, srv, "")

	for _, tc := range []struct {
		name string
		clip audio.Clip
		want error
	}{
		{"empty", audio.Clip{MIME: "audio/wav"}, audio.ErrNoAudio},
		{"unsupported", audio.Clip{MIME: "application/pdf", Data: []byte("x")}, audio.ErrUnsupportedFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tr.Transcribe(context.Background(), tc.clip, audio.Options{}); !errors.Is(err, tc.want) {
				t.Fatalf("Transcribe error = %v, want %v", err, tc.want)
			}
		})
	}
}
