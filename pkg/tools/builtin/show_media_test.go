package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// showMediaTool returns the registered tool for direct Execute() calls
// from tests. Mirrors the lookup pattern adopters use in production
// (register once via RegisterShowMedia, fetch via Registry.Get).
func showMediaTool(t *testing.T) tools.Tool {
	t.Helper()
	reg := tools.NewRegistry()
	RegisterShowMedia(reg)
	tl, ok := reg.Get(showMediaName)
	if !ok {
		t.Fatalf("show_media not registered")
	}
	return tl
}

func TestShowMedia_AcceptsDirectImageURL(t *testing.T) {
	tool := showMediaTool(t)
	out, err := tool.Execute(context.Background(),
		`{"url":"https://example.com/photo.jpg","media_type":"image","caption":"hello"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Text, "![hello](https://example.com/photo.jpg)") {
		t.Fatalf("unexpected output: %q", out.Text)
	}
}

func TestShowMedia_StripsQueryBeforeExtCheck(t *testing.T) {
	tool := showMediaTool(t)
	if _, err := tool.Execute(context.Background(),
		`{"url":"https://cdn.example.com/a.png?w=800&fm=webp","media_type":"image"}`); err != nil {
		t.Fatalf("expected query-stripped URL to pass, got: %v", err)
	}
}

func TestShowMedia_RejectsWebpageURL(t *testing.T) {
	tool := showMediaTool(t)
	_, err := tool.Execute(context.Background(),
		`{"url":"https://www.nasa.gov/humans-in-space/view-the-best-images/","media_type":"image"}`)
	if err == nil {
		t.Fatalf("expected error for webpage URL")
	}
	if !strings.Contains(err.Error(), "direct image/video URL") {
		t.Fatalf("error should mention direct URL requirement, got: %v", err)
	}
}

func TestShowMedia_RejectsNoExtension(t *testing.T) {
	tool := showMediaTool(t)
	_, err := tool.Execute(context.Background(),
		`{"url":"https://example.com/gallery/item/123","media_type":"image"}`)
	if err == nil {
		t.Fatalf("expected error for URL with no media extension")
	}
}

func TestShowMedia_AcceptsVideoURL(t *testing.T) {
	tool := showMediaTool(t)
	out, err := tool.Execute(context.Background(),
		`{"url":"https://example.com/clip.mp4","media_type":"video","caption":"demo"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Text, "<video") || !strings.Contains(out.Text, "https://example.com/clip.mp4") {
		t.Fatalf("unexpected output: %q", out.Text)
	}
}

func TestShowMedia_InlineFlagSetOnDescriptor(t *testing.T) {
	// Inline=true marks the tool's output for direct frontend rendering.
	// The refactor uses FuncToolOpts to propagate this; the regression
	// guard catches the flag if a future change drops it.
	tool := showMediaTool(t)
	if !tool.Descriptor().Inline {
		t.Fatal("expected Inline=true on show_media descriptor")
	}
}
