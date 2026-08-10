package gemini

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/audio"
	"google.golang.org/genai"
)

// Gemini has no language or vocabulary parameter, so both hints have to reach
// the model as text. Dropping them silently would degrade accuracy with
// nothing in the request to show why.
func TestBuildInstructionCarriesHints(t *testing.T) {
	base := buildInstruction(audio.Options{})
	if base != transcribeInstruction {
		t.Fatal("empty Options must not alter the instruction")
	}

	withHints := buildInstruction(audio.Options{Language: "vi", Prompt: "GopherAgent, Parakeet"})
	if !strings.HasPrefix(withHints, transcribeInstruction) {
		t.Fatal("hints must extend the base instruction, not replace it")
	}
	for _, want := range []string{"vi", "GopherAgent, Parakeet"} {
		if !strings.Contains(withHints, want) {
			t.Fatalf("instruction missing %q:\n%s", want, withHints)
		}
	}

	// A language hint alone must not smuggle in an empty vocabulary line.
	langOnly := buildInstruction(audio.Options{Language: "en"})
	if strings.Contains(langOnly, "Expect these terms") {
		t.Fatalf("empty Prompt produced a vocabulary line:\n%s", langOnly)
	}
}

// Validation runs before the client is touched, so these fail fast without a
// network call or an API key.
func TestTranscribeRejectsInvalidClipBeforeCallingAPI(t *testing.T) {
	tr := &Transcriber{}
	for _, tc := range []struct {
		name string
		clip audio.Clip
		want error
	}{
		{"empty", audio.Clip{MIME: "audio/wav"}, audio.ErrNoAudio},
		{"unsupported", audio.Clip{MIME: "application/pdf", Data: []byte("x")}, audio.ErrUnsupportedFormat},
		{"oversized", audio.Clip{MIME: "audio/wav", Data: make([]byte, maxInlineBytes+1)}, audio.ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tr.Transcribe(context.Background(), tc.clip, audio.Options{}); !errors.Is(err, tc.want) {
				t.Fatalf("Transcribe error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTranscriptFromResponse(t *testing.T) {
	content := func(text string) *genai.Content {
		return &genai.Content{Parts: []*genai.Part{{Text: text}}}
	}
	candidate := func(c *genai.Candidate) *genai.GenerateContentResponse {
		return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{c}}
	}

	t.Run("joins and trims parts", func(t *testing.T) {
		resp := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
			FinishReason: genai.FinishReasonStop,
			Content:      &genai.Content{Parts: []*genai.Part{{Text: "  hello "}, {Text: "world  "}}},
		}}}
		out, err := transcriptFromResponse(resp, audio.Options{Language: "en"})
		if err != nil {
			t.Fatalf("transcriptFromResponse: %v", err)
		}
		if out.Text != "hello world" {
			t.Fatalf("Text = %q, want %q", out.Text, "hello world")
		}
		if out.Language != "en" {
			t.Fatalf("Language = %q, want en", out.Language)
		}
		if out.Segments != nil {
			t.Fatal("generation emits no timing; Segments must stay nil")
		}
	})

	// A clean stop with nothing to say is audio that held no speech.
	t.Run("clean stop with no content is empty, not an error", func(t *testing.T) {
		out, err := transcriptFromResponse(candidate(&genai.Candidate{
			FinishReason: genai.FinishReasonStop,
		}), audio.Options{})
		if err != nil {
			t.Fatalf("transcriptFromResponse: %v", err)
		}
		if out.Text != "" {
			t.Fatalf("Text = %q, want empty", out.Text)
		}
	})

	// Regression: a safety block arrives as a non-STOP reason with nil
	// Content. Testing Content before FinishReason reported that as silence,
	// so a blocked recording was indistinguishable from a quiet one.
	t.Run("safety block with nil content errors", func(t *testing.T) {
		_, err := transcriptFromResponse(candidate(&genai.Candidate{
			FinishReason: genai.FinishReasonSafety,
		}), audio.Options{})
		if err == nil {
			t.Fatal("a safety block with nil Content must not read as an empty transcript")
		}
	})

	// A truncated answer is a partial transcript; returning its prefix as if
	// whole is the silent-prefix trap.
	t.Run("truncation errors rather than returning a prefix", func(t *testing.T) {
		_, err := transcriptFromResponse(candidate(&genai.Candidate{
			FinishReason: genai.FinishReasonMaxTokens,
			Content:      content("first half of the meeting"),
		}), audio.Options{})
		if err == nil {
			t.Fatal("truncated response must error, not return the prefix")
		}
	})

	t.Run("no candidates errors", func(t *testing.T) {
		for _, resp := range []*genai.GenerateContentResponse{nil, {}} {
			if _, err := transcriptFromResponse(resp, audio.Options{}); err == nil {
				t.Fatalf("resp %+v: want error for a response with no candidate", resp)
			}
		}
	})
}
