package history

import (
	"context"
	"log"
)

// SummaryProvider represents a very cheap LLM model (e.g., gemini-1.5-flash / gpt-4o-mini)
// exclusively tasked with finding long-term patterns in user requests.
type SummaryProvider interface {
	SummarizeBehaviors(ctx context.Context, messages []Message) (string, error)
}

// BackgroundBehaviorSummarizer handles the detached goroutine logic.
// It fires async, so the user never has to wait for this summarization piece.
func BackgroundBehaviorSummarizer(sessionKey string, history []Message, provider SummaryProvider, updateCallback func(sessionKey string, newSummary string) error) {
	// The threshold: Only summarize if we start getting deep into the context length
	const TriggerThreshold = 20

	if len(history) < TriggerThreshold {
		return
	}

	// Wait! We shouldn't summarize if we recently did it. 
	// For production, you'd add a LastSummarized turn counter. This is a simplified block.

	// Fork new goroutine so the HTTP request can return immediately
	go func() {
		// We use context.Background() to prevent the cancellation of the original HTTP 
		// request from killing this background task mid-flight.
		bgCtx := context.Background()

		log.Printf("[Background Summarizer] Forked extraction worker for session '%s'. Analyzing %d messages...\n", sessionKey, len(history))

		summaryParagraph, err := provider.SummarizeBehaviors(bgCtx, history)
		if err != nil {
			log.Printf("❌ [Background Summarizer] Analysis failed: %v", err)
			return
		}

		// Inject back to the Long-Term Memory store
		// (e.g. MySQL `UPDATE agent_sessions SET behavior_summary = ?`)
		if err := updateCallback(sessionKey, summaryParagraph); err != nil {
			log.Printf("❌ [Background Summarizer] Failed to persist memory: %v", err)
		} else {
			log.Printf("✅ [Background Summarizer] Memory persisted. New Identity/Behavior injected successfully.")
		}
	}()
}

// MockSummaryProvider is a stub to test the functionality without an API key
type MockSummaryProvider struct{}

func (m *MockSummaryProvider) SummarizeBehaviors(ctx context.Context, messages []Message) (string, error) {
	// Simulate LLM (Claude/GPT) reading history and analyzing behaviors.
	// In reality, this model would take about 2 seconds at a very low API cost.
	return "User Preferences Extract: User prefers code to be strictly typed and wrapped in interfaces. Does not tolerate SQL injection vulnerabilities. Keep responses actionable and direct without filler text.", nil
}
