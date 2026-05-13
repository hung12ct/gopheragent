package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
)

// TitleOptions configures GenerateTitle. Messages is the only required
// field; everything else has a sensible default tuned for "show this
// conversation in a sidebar" use cases.
type TitleOptions struct {
	// Messages is the conversation snippet the LLM should summarize.
	// The first user turn (and optionally first assistant reply) is the
	// canonical input — feeding the entire history is fine but wastes
	// tokens on long sessions.
	Messages []history.Message

	// MaxRunes bounds the returned title length AFTER trimming. 0 falls
	// back to 80 — a sane sidebar width.
	MaxRunes int

	// SystemPrompt overrides the library default ("4-7 word title…").
	// Empty string keeps the default; non-empty replaces it entirely.
	SystemPrompt string

	// Timeout caps the underlying GenerateStream call. 0 falls back to
	// 30s — the LLM should produce one short string fast.
	Timeout time.Duration
}

const defaultTitleSystemPrompt = "Produce a 4-7 word title that summarizes the conversation. Output ONLY the title — no quotes, no trailing punctuation, no explanation."

// GenerateTitle asks provider for a one-shot title summarizing opts.Messages.
// It runs a single GenerateStream round, drains content events, and post-
// processes the result (strip wrapping quotes, trim trailing punctuation,
// bound by rune count). Returns an error only when the provider fails or
// times out; an empty model response yields ErrEmptyTitle.
//
// Pairs with EventTypeSessionCreated: fire the title in your event
// handler / SSE relay when the session is fresh, store it next to the
// session metadata, render it in a sidebar.
func GenerateTitle(ctx context.Context, provider agent.LLMProvider, opts TitleOptions) (string, error) {
	if provider == nil {
		return "", errors.New("builtin: GenerateTitle: provider is nil")
	}
	if len(opts.Messages) == 0 {
		return "", errors.New("builtin: GenerateTitle: no messages provided")
	}

	system := opts.SystemPrompt
	if system == "" {
		system = defaultTitleSystemPrompt
	}
	maxRunes := opts.MaxRunes
	if maxRunes <= 0 {
		maxRunes = 80
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	// Strip the tool dance. Anthropic rejects message slices where an
	// assistant tool_use isn't immediately followed by its matching
	// tool_result; adopters slicing history for titling routinely produce
	// that shape ([firstUser, firstAssistant, tool, finalAssistant] is
	// fine end-to-end but blows up when only some turns are included).
	// Titles want intent + outcome, not the tool dance, so removing the
	// pair is the right semantic regardless of the provider quirk.
	sanitized := stripToolDance(opts.Messages)
	if len(sanitized) == 0 {
		return "", errors.New("builtin: GenerateTitle: no titleable messages after stripping tool-call blocks")
	}

	msgs := make([]history.Message, 0, len(sanitized)+1)
	msgs = append(msgs, history.Message{Role: "system", Content: system})
	msgs = append(msgs, sanitized...)

	streamCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ch := make(chan agent.StreamEvent, 32)
	var streamBuf strings.Builder
	var streamErr error
	var resultContent string
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := provider.GenerateStream(streamCtx, msgs, nil, ch)
		close(ch)
		if err != nil {
			streamErr = err
			return
		}
		resultContent = res.Content
	}()
	for ev := range ch {
		if ev.Type == agent.EventTypeContent {
			streamBuf.WriteString(ev.Content)
		}
	}
	<-done
	if streamErr != nil {
		return "", fmt.Errorf("builtin: GenerateTitle: %w", streamErr)
	}
	// Some providers populate Content via the LLMResult rather than emitting
	// content events — fall back to it when the stream was empty. Done after
	// <-done so the main goroutine is the only writer to streamBuf.
	if streamBuf.Len() == 0 && resultContent != "" {
		streamBuf.WriteString(resultContent)
	}

	title := normalizeTitle(streamBuf.String(), maxRunes)
	if title == "" {
		return "", ErrEmptyTitle
	}
	return title, nil
}

// ErrEmptyTitle is returned when the provider produced no usable text.
// Callers typically fall back to a placeholder like "New conversation".
var ErrEmptyTitle = errors.New("builtin: GenerateTitle: model returned empty title")

// stripToolDance returns a copy of in with role:"tool" messages removed
// and ToolCalls cleared from assistant messages. An assistant message
// whose only content was a tool_use block (no text / parts) is dropped
// entirely. Other roles pass through unchanged.
//
// This keeps titling robust when adopters feed a sub-slice of the full
// conversation (e.g. [firstUser, firstAssistant, tool, finalAssistant])
// — Anthropic otherwise rejects the call because a tool_use needs its
// tool_result in the immediately-next message and the slice boundaries
// can break that pairing.
func stripToolDance(in []history.Message) []history.Message {
	out := make([]history.Message, 0, len(in))
	for _, m := range in {
		switch m.Role {
		case "tool":
			continue
		case "assistant":
			if len(m.ToolCalls) == 0 {
				out = append(out, m)
				continue
			}
			cleaned := m
			cleaned.ToolCalls = nil
			if cleaned.Content == "" && len(cleaned.Parts) == 0 {
				continue
			}
			out = append(out, cleaned)
		default:
			out = append(out, m)
		}
	}
	return out
}

// normalizeTitle strips wrapping quotes, trims trailing punctuation that
// the model occasionally adds despite instructions, collapses internal
// whitespace, and bounds by maxRunes (cutting at the last space when
// possible to avoid mid-word truncation).
func normalizeTitle(raw string, maxRunes int) string {
	t := strings.TrimSpace(raw)
	t = strings.Trim(t, `"'`+"`")
	t = strings.TrimRight(t, ".!?,;:")
	t = strings.TrimSpace(t)
	t = strings.Join(strings.Fields(t), " ")

	if utf8.RuneCountInString(t) <= maxRunes {
		return t
	}
	runes := []rune(t)
	cut := runes[:maxRunes]
	if idx := strings.LastIndex(string(cut), " "); idx > 0 {
		return string(cut[:idx])
	}
	return string(cut)
}
