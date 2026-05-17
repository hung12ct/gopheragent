package agent

import "context"

// sessionKeyContextKey is the unexported context key under which the active
// session key is propagated to tools. Using an unexported struct{} key is the
// Go idiom for ctx values — collisions with foreign packages are impossible
// and the key is invisible to godoc.
type sessionKeyContextKey struct{}

// WithSessionKey returns a new context carrying the session key. Tools and
// integrations should call SessionKeyFromContext rather than building the
// lookup key themselves.
func WithSessionKey(ctx context.Context, sessionKey string) context.Context {
	return context.WithValue(ctx, sessionKeyContextKey{}, sessionKey)
}

// SessionKeyFromContext returns the session key stamped by WithSessionKey.
// The second return is false when no key is present (typically only in tests
// or when a tool is invoked outside the agent loop).
func SessionKeyFromContext(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(sessionKeyContextKey{}).(string)
	return s, ok
}
