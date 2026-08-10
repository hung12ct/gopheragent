package openai

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
)

// Tests cover the adapter's pure conversion helper — going through the full
// GenerateStream path would require mocking the SDK's streaming client, which
// is not worth the complexity for what is fundamentally a struct-shape
// transformation.

var pngBytes = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

func TestOpenAI_CaptionPlusImageURL(t *testing.T) {
	parts, err := partsFromMediaParts("what's in this?", []history.MediaPart{
		history.NewImagePartURL("image/jpeg", "https://example.com/a.jpg"),
	})
	if err != nil {
		t.Fatalf("partsFromMediaParts: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "what's in this?" {
		t.Fatalf("expected leading text part, got %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://example.com/a.jpg" {
		t.Fatalf("expected image_url part, got %+v", parts[1])
	}
}

func TestOpenAI_BytesBecomeDataURI(t *testing.T) {
	parts, err := partsFromMediaParts("", []history.MediaPart{
		history.NewImagePartBytes("image/png", pngBytes),
	})
	if err != nil {
		t.Fatalf("partsFromMediaParts: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	if parts[0].ImageURL.URL != want {
		t.Fatalf("expected data URI, got %q", parts[0].ImageURL.URL)
	}
}

// An empty text part carries nothing to lose, so it is dropped. Everything
// else this adapter cannot render fails the message: dropping it would send a
// prompt about media the model never received and return a confident answer.
func TestOpenAI_DropsEmptyTextButRejectsUnrenderable(t *testing.T) {
	parts, err := partsFromMediaParts("", []history.MediaPart{
		{Type: history.PartText, Text: ""},
		history.NewImagePartURL("image/png", "https://x/a"),
	})
	if err != nil {
		t.Fatalf("empty text part must be dropped, not rejected: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part after dropping empty text, got %d", len(parts))
	}

	for _, tc := range []struct {
		name string
		part history.MediaPart
	}{
		{"image without payload", history.MediaPart{Type: history.PartImage, MIME: "image/png"}},
		{"unknown part type", history.MediaPart{Type: history.PartType("audio"), MIME: "audio/wav", Data: pngBytes}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := partsFromMediaParts("caption", []history.MediaPart{tc.part}); !errors.Is(err, agent.ErrUnrenderablePart) {
				t.Fatalf("error = %v, want ErrUnrenderablePart", err)
			}
		})
	}
}

// Parts on a non-user role are rejected at the message level: OpenAI has no
// multimodal assistant or tool content to render them into.
func TestOpenAI_RejectsPartsOnNonUserRole(t *testing.T) {
	p, err := New("test-key", "gpt-4o")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	memory := []history.Message{{
		Role:  "assistant",
		Parts: []history.MediaPart{history.NewImagePartURL("image/png", "https://x/a")},
	}}
	_, err = p.GenerateStream(context.Background(), memory, nil, make(chan agent.StreamEvent, 1))
	if !errors.Is(err, agent.ErrUnrenderablePart) {
		t.Fatalf("GenerateStream error = %v, want ErrUnrenderablePart", err)
	}
}
