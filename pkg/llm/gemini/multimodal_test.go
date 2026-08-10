package gemini

import (
	"context"
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

func TestGemini_BytesBecomeInlineData(t *testing.T) {
	parts, err := partsFromMediaParts("look", []history.MediaPart{
		history.NewImagePartBytes("image/png", pngBytes),
	})
	if err != nil {
		t.Fatalf("partsFromMediaParts: %v", err)
	}
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
	parts, err := partsFromMediaParts("", []history.MediaPart{
		history.NewImagePartURL("image/jpeg", "gs://bucket/obj.jpg"),
	})
	if err != nil {
		t.Fatalf("partsFromMediaParts: %v", err)
	}
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
	parts, err := partsFromMediaParts("", []history.MediaPart{
		history.NewTextPart("A"),
		history.NewImagePartBytes("image/png", pngBytes),
		history.NewTextPart("B"),
	})
	if err != nil {
		t.Fatalf("partsFromMediaParts: %v", err)
	}
	if len(parts) != 3 || parts[0].Text != "A" || parts[2].Text != "B" {
		t.Fatalf("expected interleaved text parts preserved, got %+v", parts)
	}
}

// An empty text part carries nothing to lose, so it is dropped. Everything
// else this adapter cannot render fails the message rather than answering
// from media the model never received.
func TestGemini_DropsEmptyTextButRejectsUnrenderable(t *testing.T) {
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

// Parts on a non-user role are rejected at the message level. The branches
// for system/assistant/tool read Content alone, so letting them through would
// answer from media the model never received.
func TestGemini_RejectsPartsOnNonUserRole(t *testing.T) {
	p, err := New("test-key", "gemini-2.5-flash")
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
