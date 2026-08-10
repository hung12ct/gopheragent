package anthropic

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
)

// Tests cover the adapter's pure conversion helper — going through the full
// GenerateStream path would require mocking the SDK's streaming client, which
// is not worth the complexity for what is fundamentally a struct-shape
// transformation.

var pngBytes = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

func TestAnthropic_CaptionPlusImageURL(t *testing.T) {
	blocks, err := blocksFromMediaParts("describe", []history.MediaPart{
		history.NewImagePartURL("image/jpeg", "https://example.com/a.jpg"),
	})
	if err != nil {
		t.Fatalf("blocksFromMediaParts: %v", err)
	}
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
	blocks, err := blocksFromMediaParts("", []history.MediaPart{
		history.NewImagePartBytes("image/png", pngBytes),
	})
	if err != nil {
		t.Fatalf("blocksFromMediaParts: %v", err)
	}
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
	blocks, err := blocksFromMediaParts("", []history.MediaPart{
		{Type: history.PartImage, Data: pngBytes},
	})
	if err != nil {
		t.Fatalf("blocksFromMediaParts: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block")
	}
	if !strings.HasPrefix(string(blocks[0].OfImage.Source.OfBase64.MediaType), "image/") {
		t.Fatalf("expected image/* default, got %q", blocks[0].OfImage.Source.OfBase64.MediaType)
	}
}

// An empty text part carries nothing to lose, so it is dropped. Everything
// else this adapter cannot render fails the message: Anthropic has no audio
// or video block at all, and dropping one would answer confidently from media
// the model never received.
func TestAnthropic_DropsEmptyTextButRejectsUnrenderable(t *testing.T) {
	blocks, err := blocksFromMediaParts("", []history.MediaPart{
		{Type: history.PartText, Text: ""},
		history.NewImagePartURL("image/png", "https://x/a"),
	})
	if err != nil {
		t.Fatalf("empty text part must be dropped, not rejected: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block after dropping empty text, got %d", len(blocks))
	}

	for _, tc := range []struct {
		name string
		part history.MediaPart
	}{
		{"image without payload", history.MediaPart{Type: history.PartImage, MIME: "image/png"}},
		{"audio part", history.MediaPart{Type: history.PartType("audio"), MIME: "audio/wav", Data: pngBytes}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := blocksFromMediaParts("caption", []history.MediaPart{tc.part}); !errors.Is(err, agent.ErrUnrenderablePart) {
				t.Fatalf("error = %v, want ErrUnrenderablePart", err)
			}
		})
	}
}

// Parts on a non-user role are rejected at the message level. The branches
// for system/assistant/tool read Content alone, so letting them through would
// answer from media the model never received — the same silent drop this
// adapter used to do inside blocksFromMediaParts.
func TestAnthropic_RejectsPartsOnNonUserRole(t *testing.T) {
	p, err := New("test-key", "claude-sonnet-4-20250514")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, role := range []string{"system", "assistant", "tool"} {
		t.Run(role, func(t *testing.T) {
			memory := []history.Message{{
				Role:  role,
				Parts: []history.MediaPart{history.NewImagePartURL("image/png", "https://x/a")},
			}}
			_, err := p.GenerateStream(context.Background(), memory, nil, make(chan agent.StreamEvent, 1))
			if !errors.Is(err, agent.ErrUnrenderablePart) {
				t.Fatalf("GenerateStream error = %v, want ErrUnrenderablePart", err)
			}
		})
	}
}
