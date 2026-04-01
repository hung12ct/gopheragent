package history

import (
	"context"
	"log"
)

// SummaryProvider represents a very cheap LLM model (e.g., gemini-1.5-flash / gpt-4o-mini)
// exclusively tasked with finding long-term patterns in user requests.
//
// previousSummary is the existing behavioral profile (empty string on first call).
// The implementation should merge new evidence from newMessages into previousSummary
// rather than rebuilding from scratch, saving tokens and preserving accumulated knowledge.
type SummaryProvider interface {
	SummarizeBehaviors(ctx context.Context, newMessages []Message, previousSummary string) (string, error)
}

// BackgroundBehaviorSummarizer fires async summarization for a session.
// Only newMessages (since last summarization) are sent to the provider, together
// with the existing previousSummary so the model can do an incremental merge.
func BackgroundBehaviorSummarizer(
	sessionKey string,
	newMessages []Message,
	previousSummary string,
	provider SummaryProvider,
	updateCallback func(sessionKey string, newSummary string) error,
) {
	if len(newMessages) == 0 {
		return
	}

	go func() {
		bgCtx := context.Background()
		log.Printf("[Background Summarizer] session=%q new_msgs=%d merging with existing profile",
			sessionKey, len(newMessages))

		updated, err := provider.SummarizeBehaviors(bgCtx, newMessages, previousSummary)
		if err != nil {
			log.Printf("[Background Summarizer] failed for session %q: %v", sessionKey, err)
			return
		}

		if err := updateCallback(sessionKey, updated); err != nil {
			log.Printf("[Background Summarizer] persist failed for session %q: %v", sessionKey, err)
		} else {
			log.Printf("[Background Summarizer] profile updated for session %q", sessionKey)
		}
	}()
}

// MockSummaryProvider is a stub for tests — no API key required.
type MockSummaryProvider struct{}

func (m *MockSummaryProvider) SummarizeBehaviors(_ context.Context, _ []Message, previous string) (string, error) {
	if previous != "" {
		return previous + " (updated)", nil
	}
	return "User prefers strictly typed Go code, direct responses, no filler text.", nil
}
