package gemini

import (
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// Tests cover the adapter's pure conversion helper — going through the full
// GenerateStream path would require mocking the SDK's streaming client, which
// is not worth the complexity for what is fundamentally a struct-shape
// transformation.

var pngBytes = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

func TestGemini_BytesBecomeInlineData(t *testing.T) {
	parts := partsFromMediaParts("look", []history.MediaPart{
		history.NewImagePartBytes("image/png", pngBytes),
	})
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].Text != "look" {
		t.Fatalf("expected leading text, got %+v", parts[0])
	}
	if parts[1].InlineData == nil {
		t.Fatalf("expected inline data, got %+v", parts[1])
	}
	if parts[1].InlineData.MIMEType != "image/png" {
		t.Fatalf("expected image/png, got %q", parts[1].InlineData.MIMEType)
	}
}

func TestGemini_URLBecomesFileData(t *testing.T) {
	parts := partsFromMediaParts("", []history.MediaPart{
		history.NewImagePartURL("image/jpeg", "gs://bucket/obj.jpg"),
	})
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if parts[0].FileData == nil {
		t.Fatalf("expected file data, got %+v", parts[0])
	}
	if parts[0].FileData.FileURI != "gs://bucket/obj.jpg" {
		t.Fatalf("expected gs URI, got %q", parts[0].FileData.FileURI)
	}
}

func TestGemini_InterleavedTextAndImage(t *testing.T) {
	parts := partsFromMediaParts("", []history.MediaPart{
		history.NewTextPart("A"),
		history.NewImagePartBytes("image/png", pngBytes),
		history.NewTextPart("B"),
	})
	if len(parts) != 3 || parts[0].Text != "A" || parts[2].Text != "B" {
		t.Fatalf("expected interleaved text parts preserved, got %+v", parts)
	}
}
