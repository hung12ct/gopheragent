package openai

import (
	"encoding/base64"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// Tests cover the adapter's pure conversion helper — going through the full
// GenerateStream path would require mocking the SDK's streaming client, which
// is not worth the complexity for what is fundamentally a struct-shape
// transformation.

var pngBytes = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

func TestOpenAI_CaptionPlusImageURL(t *testing.T) {
	parts := partsFromMediaParts("what's in this?", []history.MediaPart{
		history.NewImagePartURL("image/jpeg", "https://example.com/a.jpg"),
	})
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
	parts := partsFromMediaParts("", []history.MediaPart{
		history.NewImagePartBytes("image/png", pngBytes),
	})
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	if parts[0].ImageURL.URL != want {
		t.Fatalf("expected data URI, got %q", parts[0].ImageURL.URL)
	}
}

func TestOpenAI_SkipsEmpty(t *testing.T) {
	parts := partsFromMediaParts("", []history.MediaPart{
		{Type: history.PartText, Text: ""},                  // empty text — drop
		{Type: history.PartImage, MIME: "image/png"},        // no URL, no Data — drop
		history.NewImagePartURL("image/png", "https://x/a"), // keep
	})
	if len(parts) != 1 {
		t.Fatalf("expected 1 part after filtering, got %d", len(parts))
	}
}
