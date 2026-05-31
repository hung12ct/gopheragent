package llm

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// Tests cover the three provider adapters' pure conversion helpers —
// going through the full GenerateStream path would require mocking each
// SDK's streaming client, which is not worth the complexity for what is
// fundamentally a struct-shape transformation.

var pngBytes = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

// ── OpenAI ────────────────────────────────────────────────────────────────

func TestOpenAI_CaptionPlusImageURL(t *testing.T) {
	parts := openAIPartsFromMediaParts("what's in this?", []history.MediaPart{
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
	parts := openAIPartsFromMediaParts("", []history.MediaPart{
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
	parts := openAIPartsFromMediaParts("", []history.MediaPart{
		{Type: history.PartText, Text: ""},                  // empty text — drop
		{Type: history.PartImage, MIME: "image/png"},        // no URL, no Data — drop
		history.NewImagePartURL("image/png", "https://x/a"), // keep
	})
	if len(parts) != 1 {
		t.Fatalf("expected 1 part after filtering, got %d", len(parts))
	}
}

// ── Anthropic ─────────────────────────────────────────────────────────────

func TestAnthropic_CaptionPlusImageURL(t *testing.T) {
	blocks := anthropicBlocksFromMediaParts("describe", []history.MediaPart{
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
	blocks := anthropicBlocksFromMediaParts("", []history.MediaPart{
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
	blocks := anthropicBlocksFromMediaParts("", []history.MediaPart{
		{Type: history.PartImage, Data: pngBytes},
	})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block")
	}
	if !strings.HasPrefix(string(blocks[0].OfImage.Source.OfBase64.MediaType), "image/") {
		t.Fatalf("expected image/* default, got %q", blocks[0].OfImage.Source.OfBase64.MediaType)
	}
}

// ── Gemini ────────────────────────────────────────────────────────────────

func TestGemini_BytesBecomeInlineData(t *testing.T) {
	parts := geminiPartsFromMediaParts("look", []history.MediaPart{
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
	parts := geminiPartsFromMediaParts("", []history.MediaPart{
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
	parts := geminiPartsFromMediaParts("", []history.MediaPart{
		history.NewTextPart("A"),
		history.NewImagePartBytes("image/png", pngBytes),
		history.NewTextPart("B"),
	})
	if len(parts) != 3 || parts[0].Text != "A" || parts[2].Text != "B" {
		t.Fatalf("expected interleaved text parts preserved, got %+v", parts)
	}
}
