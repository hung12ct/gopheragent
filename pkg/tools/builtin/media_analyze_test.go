package builtin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubMediaAnalyzer struct {
	lastMedia  string
	lastPrompt string
	resp       string
	err        error
}

func (s *stubMediaAnalyzer) Analyze(_ context.Context, media, prompt string) (string, error) {
	s.lastMedia = media
	s.lastPrompt = prompt
	return s.resp, s.err
}

func TestMediaAnalyzeTool_DelegatesToAnalyzer(t *testing.T) {
	a := &stubMediaAnalyzer{resp: "a red square"}
	tool := NewMediaAnalyzeTool(a)

	out, err := tool.Execute(context.Background(), `{"media":"https://x/y.png","prompt":"what is it?"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "a red square" {
		t.Fatalf("output mismatch: %q", out)
	}
	if a.lastMedia != "https://x/y.png" || a.lastPrompt != "what is it?" {
		t.Fatalf("analyzer got wrong args: %+v", a)
	}
}

func TestMediaAnalyzeTool_AcceptsVideoURL(t *testing.T) {
	a := &stubMediaAnalyzer{resp: "a cat video"}
	tool := NewMediaAnalyzeTool(a)

	out, err := tool.Execute(context.Background(), `{"media":"gs://my-bucket/cat.mp4","prompt":"summarise the video"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "a cat video" {
		t.Fatalf("output mismatch: %q", out)
	}
	if a.lastMedia != "gs://my-bucket/cat.mp4" {
		t.Fatalf("unexpected media arg: %q", a.lastMedia)
	}
}

func TestMediaAnalyzeTool_RequiresMediaAndPrompt(t *testing.T) {
	tool := NewMediaAnalyzeTool(&stubMediaAnalyzer{})
	if _, err := tool.Execute(context.Background(), `{"media":"","prompt":"p"}`); err == nil {
		t.Fatal("expected media required error")
	}
	if _, err := tool.Execute(context.Background(), `{"media":"x","prompt":""}`); err == nil {
		t.Fatal("expected prompt required error")
	}
}

func TestMediaAnalyzeTool_ErrorsWhenAnalyzerMissing(t *testing.T) {
	tool := &MediaAnalyzeTool{}
	_, err := tool.Execute(context.Background(), `{"media":"x","prompt":"p"}`)
	if err == nil || !strings.Contains(err.Error(), "analyzer") {
		t.Fatalf("expected analyzer error, got %v", err)
	}
}

func TestMediaAnalyzeTool_PropagatesAnalyzerError(t *testing.T) {
	a := &stubMediaAnalyzer{err: errors.New("vision down")}
	tool := NewMediaAnalyzeTool(a)
	_, err := tool.Execute(context.Background(), `{"media":"x","prompt":"p"}`)
	if err == nil || !strings.Contains(err.Error(), "vision down") {
		t.Fatalf("expected propagation, got %v", err)
	}
}
