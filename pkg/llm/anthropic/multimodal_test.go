package anthropic

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// Tests cover the adapter's pure conversion helper — going through the full
// GenerateStream path would require mocking the SDK's streaming client, which
// is not worth the complexity for what is fundamentally a struct-shape
// transformation.

var pngBytes = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

func TestAnthropic_CaptionPlusImageURL(t *testing.T) {
	blocks := blocksFromMediaParts("describe", []history.MediaPart{
		history.NewImagePartURL("image/jpeg", "https://example.com/a.jpg"),
	})
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].OfText == nil || blocks[0].OfText.Text != "describe" {
		t.Fatalf("expected text block, got %+v", blocks[0])
	}
	if blocks[1].OfImage == nil {
		t.Fatalf("expected image block, got %+v", blocks[1])
	}
	if blocks[1].OfImage.Source.OfURL == nil || blocks[1].OfImage.Source.OfURL.URL != "https://example.com/a.jpg" {
		t.Fatalf("expected URL image source, got %+v", blocks[1].OfImage.Source)
	}
}

func TestAnthropic_BytesUseBase64Source(t *testing.T) {
	blocks := blocksFromMediaParts("", []history.MediaPart{
		history.NewImagePartBytes("image/png", pngBytes),
	})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	src := blocks[0].OfImage.Source.OfBase64
	if src == nil {
		t.Fatalf("expected base64 source")
	}
	if string(src.MediaType) != "image/png" {
		t.Fatalf("expected image/png, got %q", src.MediaType)
	}
	want := base64.StdEncoding.EncodeToString(pngBytes)
	if src.Data != want {
		t.Fatalf("base64 mismatch")
	}
}

func TestAnthropic_DefaultMIME(t *testing.T) {
	// No MIME on a bytes part — adapter must default to image/png.
	blocks := blocksFromMediaParts("", []history.MediaPart{
		{Type: history.PartImage, Data: pngBytes},
	})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block")
	}
	if !strings.HasPrefix(string(blocks[0].OfImage.Source.OfBase64.MediaType), "image/") {
		t.Fatalf("expected image/* default, got %q", blocks[0].OfImage.Source.OfBase64.MediaType)
	}
}
