package builtin

import (
	"context"
	"strings"
	"testing"
)

func TestShowMedia_AcceptsDirectImageURL(t *testing.T) {
	tool := NewShowMediaTool()
	out, err := tool.Execute(context.Background(),
		`{"url":"https://example.com/photo.jpg","media_type":"image","caption":"hello"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "![hello](https://example.com/photo.jpg)") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestShowMedia_StripsQueryBeforeExtCheck(t *testing.T) {
	tool := NewShowMediaTool()
	if _, err := tool.Execute(context.Background(),
		`{"url":"https://cdn.example.com/a.png?w=800&fm=webp","media_type":"image"}`); err != nil {
		t.Fatalf("expected query-stripped URL to pass, got: %v", err)
	}
}

func TestShowMedia_RejectsWebpageURL(t *testing.T) {
	tool := NewShowMediaTool()
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
	tool := NewShowMediaTool()
	_, err := tool.Execute(context.Background(),
		`{"url":"https://example.com/gallery/item/123","media_type":"image"}`)
	if err == nil {
		t.Fatalf("expected error for URL with no media extension")
	}
}

func TestShowMedia_AcceptsVideoURL(t *testing.T) {
	tool := NewShowMediaTool()
	out, err := tool.Execute(context.Background(),
		`{"url":"https://example.com/clip.mp4","media_type":"video","caption":"demo"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "<video") || !strings.Contains(out, "https://example.com/clip.mp4") {
		t.Fatalf("unexpected output: %q", out)
	}
}
