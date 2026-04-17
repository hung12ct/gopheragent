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
// Returns the generated new session key. Fails if the session has no user
// messages or the underlying Fork call fails.
func ForkAtLastUser(ctx context.Context, sm SessionManager, sessionKey string) (string, error) {
	msgs := sm.GetHistory(ctx, sessionKey)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return sm.Fork(ctx, sessionKey, i)
		}
	}
	return "", fmt.Errorf("agent: ForkAtLastUser: session %q has no user messages", sessionKey)
}
