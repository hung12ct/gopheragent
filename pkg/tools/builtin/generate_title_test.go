package builtin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// titleProvider is a minimal LLMProvider stub for GenerateTitle tests.
// It can stream the response in chunks (exercise event-buffering path) or
// only set Content on the result (exercise fallback path).
type titleProvider struct {
	streamChunks []string
	resultText   string
	err          error
}

func (p *titleProvider) GenerateStream(_ context.Context, _ []history.Message, _ *tools.Registry, ch chan<- agent.StreamEvent) (agent.LLMResult, error) {
	if p.err != nil {
		return agent.LLMResult{}, p.err
	}
	for _, chunk := range p.streamChunks {
		ch <- agent.StreamEvent{Type: agent.EventTypeContent, Content: chunk}
	}
	return agent.LLMResult{Content: p.resultText}, nil
}

func TestGenerateTitle_StreamsContentEvents(t *testing.T) {
	p := &titleProvider{streamChunks: []string{"Quarterly ", "Marketing ", "Performance Review"}}
	title, err := GenerateTitle(context.Background(), p, TitleOptions{
		Messages: []history.Message{{Role: "user", Content: "summarize Q4"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "Quarterly Marketing Performance Review" {
		t.Fatalf("unexpected title: %q", title)
	}
}

func TestGenerateTitle_FallsBackToResultContent(t *testing.T) {
	// Some providers don't emit content events; GenerateTitle must use
	// LLMResult.Content as a fallback.
	p := &titleProvider{resultText: "Direct Result Title"}
	title, err := GenerateTitle(context.Background(), p, TitleOptions{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "Direct Result Title" {
		t.Fatalf("unexpected title: %q", title)
	}
}

func TestGenerateTitle_NormalizesQuotesAndTrailingPunctuation(t *testing.T) {
	p := &titleProvider{streamChunks: []string{`"Yearly Recap."`}}
	title, err := GenerateTitle(context.Background(), p, TitleOptions{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "Yearly Recap" {
		t.Fatalf("expected stripped title, got %q", title)
	}
}

func TestGenerateTitle_BoundsAtRuneCountWithoutSplittingWords(t *testing.T) {
	long := "An overly verbose conversation summary about quarterly marketing pipeline performance"
	p := &titleProvider{streamChunks: []string{long}}
	title, err := GenerateTitle(context.Background(), p, TitleOptions{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
		MaxRunes: 30,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len([]rune(title)) > 30 {
		t.Fatalf("title exceeds bound: %q (%d runes)", title, len([]rune(title)))
	}
	if strings.HasSuffix(title, " ") {
		t.Fatalf("title left trailing space after word-boundary cut: %q", title)
	}
}

func TestGenerateTitle_EmptyResponseReturnsErrEmptyTitle(t *testing.T) {
	p := &titleProvider{}
	_, err := GenerateTitle(context.Background(), p, TitleOptions{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, ErrEmptyTitle) {
		t.Fatalf("expected ErrEmptyTitle, got %v", err)
	}
}

func TestGenerateTitle_PropagatesProviderError(t *testing.T) {
	boom := errors.New("provider down")
	p := &titleProvider{err: boom}
	_, err := GenerateTitle(context.Background(), p, TitleOptions{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped provider error, got %v", err)
	}
}

func TestGenerateTitle_RejectsNilProviderAndEmptyMessages(t *testing.T) {
	if _, err := GenerateTitle(context.Background(), nil, TitleOptions{Messages: []history.Message{{Role: "user"}}}); err == nil {
		t.Fatal("expected error for nil provider")
	}
	p := &titleProvider{streamChunks: []string{"x"}}
	if _, err := GenerateTitle(context.Background(), p, TitleOptions{}); err == nil {
		t.Fatal("expected error for empty messages")
	}
}

func TestGenerateTitle_HonorsTimeout(t *testing.T) {
	// A provider that never returns; the explicit Timeout must trigger
	// ctx cancellation.
	hung := &hangProvider{}
	_, err := GenerateTitle(context.Background(), hung, TitleOptions{
		Messages: []history.Message{{Role: "user", Content: "hi"}},
		Timeout:  20 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

type hangProvider struct{}

func (h *hangProvider) GenerateStream(ctx context.Context, _ []history.Message, _ *tools.Registry, _ chan<- agent.StreamEvent) (agent.LLMResult, error) {
	<-ctx.Done()
	return agent.LLMResult{}, ctx.Err()
}
