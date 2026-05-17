package agent

import (
	"context"
	"fmt"
)

// ForkAtLastUser creates a branch that keeps everything before the most recent
// user message in sessionKey. This is the "redo my last turn" shortcut: after
// forking, the caller can append a new user message to the new session and run
// the loop again.
//
// Requires sm to implement SessionForker; returns an error if it does not.
// Fails if the session has no user messages or the underlying Fork fails.
func ForkAtLastUser(ctx context.Context, sm SessionManager, sessionKey string) (string, error) {
	forker, ok := sm.(SessionForker)
	if !ok {
		return "", fmt.Errorf("agent: ForkAtLastUser: session manager does not implement SessionForker")
	}
	msgs, err := sm.History(ctx, sessionKey)
	if err != nil {
		return "", fmt.Errorf("agent: ForkAtLastUser: load history: %w", err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return forker.Fork(ctx, sessionKey, i)
		}
	}
	return "", fmt.Errorf("agent: ForkAtLastUser: session %q has no user messages", sessionKey)
}
