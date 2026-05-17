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
// only set Content on the result (exercise fallback path). got captures
// the message slice GenerateTitle forwarded — used to assert sanitization.
type titleProvider struct {
	streamChunks []string
	resultText   string
	err          error
	got          []history.Message
}

func (p *titleProvider) GenerateStream(_ context.Context, msgs []history.Message, _ *tools.Registry, ch chan<- agent.StreamEvent) (agent.LLMResult, error) {
	p.got = append([]history.Message(nil), msgs...)
	if p.err != nil {
		return agent.LLMResult{}, p.err
	}
	for _, chunk := range p.streamChunks {
		ch <- agent.Event(agent.ContentEvent{Text: chunk})
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

func TestGenerateTitle_StripsToolBlocksFromHistory(t *testing.T) {
	// Mirrors Phin's slice: [firstUser, firstAssistant-with-toolcall, tool,
	// finalAssistant]. Before the fix, this propagated tool_use blocks
	// without paired tool_result blocks to Anthropic and 400'd. After
	// sanitization, the provider sees system + user + (preserved
	// intermediate assistant text) + final assistant only.
	p := &titleProvider{streamChunks: []string{"Top Customers Report"}}
	title, err := GenerateTitle(context.Background(), p, TitleOptions{
		Messages: []history.Message{
			{Role: "user", Content: "show me top customers"},
			{Role: "assistant", Content: "Checking the warehouse...", ToolCalls: []history.ToolCall{{ID: "toolu_1", Name: "call_sql_agent", Arguments: `{"query":"top customers"}`}}},
			{Role: "tool", ToolCallID: "toolu_1", Content: `{"rows":[]}`},
			{Role: "assistant", Content: "Top customers are X, Y, Z."},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "Top Customers Report" {
		t.Fatalf("unexpected title: %q", title)
	}
	// p.got[0] is the injected system prompt; the rest must be free of
	// tool_use / tool_result residue.
	if len(p.got) < 1 || p.got[0].Role != "system" {
		t.Fatalf("expected system prompt first, got %+v", p.got)
	}
	for i, m := range p.got[1:] {
		if m.Role == "tool" {
			t.Fatalf("p.got[%d] is a tool message; should have been stripped", i+1)
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			t.Fatalf("p.got[%d] still carries ToolCalls: %+v", i+1, m.ToolCalls)
		}
	}
	// The final assistant turn must still be in the forwarded slice,
	// followed by the library-appended user nudge (Anthropic requires the
	// conversation to end with a user message).
	if len(p.got) < 2 {
		t.Fatalf("expected forwarded slice to include sanitized turns, got %+v", p.got)
	}
	last := p.got[len(p.got)-1]
	if last.Role != "user" {
		t.Fatalf("expected trailing user nudge, got %+v", last)
	}
	foundFinal := false
	for _, m := range p.got {
		if m.Role == "assistant" && m.Content == "Top customers are X, Y, Z." {
			foundFinal = true
			break
		}
	}
	if !foundFinal {
		t.Fatalf("final assistant lost from forwarded slice: %+v", p.got)
	}
}

func TestGenerateTitle_AppendsUserNudgeWhenHistoryEndsWithAssistant(t *testing.T) {
	// Anthropic 400s on conversations ending in assistant. GenerateTitle
	// appends a library-owned user nudge so adopters don't each rediscover
	// the rule and copy-paste their own phrasing.
	p := &titleProvider{streamChunks: []string{"Customer Lookup"}}
	_, err := GenerateTitle(context.Background(), p, TitleOptions{
		Messages: []history.Message{
			{Role: "user", Content: "look up customer 42"},
			{Role: "assistant", Content: "Customer 42 is Acme Corp."},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.got) != 4 || p.got[0].Role != "system" || p.got[1].Role != "user" || p.got[2].Role != "assistant" {
		t.Fatalf("expected [system, user, assistant, user-nudge], got %+v", p.got)
	}
	nudge := p.got[3]
	if nudge.Role != "user" || nudge.Content == "" {
		t.Fatalf("expected user-role nudge with content, got %+v", nudge)
	}
}

func TestGenerateTitle_DoesNotAppendNudgeWhenHistoryEndsWithUser(t *testing.T) {
	// Mid-conversation retitle: the slice already ends with user.
	// Appending another user turn would duplicate it and confuse the model.
	p := &titleProvider{streamChunks: []string{"Followup Question"}}
	_, err := GenerateTitle(context.Background(), p, TitleOptions{
		Messages: []history.Message{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "answer"},
			{Role: "user", Content: "second"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// system + user + assistant + user → no nudge appended.
	if len(p.got) != 4 {
		t.Fatalf("expected 4 messages (no extra nudge), got %d: %+v", len(p.got), p.got)
	}
	if p.got[3].Role != "user" || p.got[3].Content != "second" {
		t.Fatalf("user nudge incorrectly appended: %+v", p.got[3])
	}
}

func TestGenerateTitle_DropsEmptyAssistantAfterStrippingToolCalls(t *testing.T) {
	// Assistant emitted only a tool_use (no narration). After clearing
	// ToolCalls, the message has no Content / Parts — drop it entirely
	// rather than send an empty assistant turn the provider will reject.
	p := &titleProvider{streamChunks: []string{"Untitled Query"}}
	_, err := GenerateTitle(context.Background(), p, TitleOptions{
		Messages: []history.Message{
			{Role: "user", Content: "list tables"},
			{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "toolu_2", Name: "call_sql_agent", Arguments: "{}"}}},
			{Role: "tool", ToolCallID: "toolu_2", Content: "ok"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// system + user only; the empty-after-strip assistant and the tool
	// row must both be gone.
	if len(p.got) != 2 || p.got[0].Role != "system" || p.got[1].Role != "user" {
		t.Fatalf("expected [system, user], got %+v", p.got)
	}
}

func TestGenerateTitle_ErrorsWhenEveryMessageStripped(t *testing.T) {
	p := &titleProvider{streamChunks: []string{"ignored"}}
	_, err := GenerateTitle(context.Background(), p, TitleOptions{
		Messages: []history.Message{
			{Role: "assistant", ToolCalls: []history.ToolCall{{ID: "toolu_3", Name: "x", Arguments: "{}"}}},
			{Role: "tool", ToolCallID: "toolu_3", Content: "done"},
		},
	})
	if err == nil {
		t.Fatal("expected error when no titleable messages remain")
	}
	if !strings.Contains(err.Error(), "stripping tool-call blocks") {
		t.Fatalf("expected stripped-empty error, got %v", err)
	}
}

type hangProvider struct{}

func (h *hangProvider) GenerateStream(ctx context.Context, _ []history.Message, _ *tools.Registry, _ chan<- agent.StreamEvent) (agent.LLMResult, error) {
	<-ctx.Done()
	return agent.LLMResult{}, ctx.Err()
}
