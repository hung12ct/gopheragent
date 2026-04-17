package agent

import "context"

// SubAgentEmitter forwards a StreamEvent from a sub-agent into its parent
// agent's stream. Sends must be non-blocking: a stalled parent consumer must
// never be able to deadlock a sub-agent.
type SubAgentEmitter func(ev StreamEvent)

type subAgentEmitterKey struct{}

// WithSubAgentEmitter injects an emitter into ctx so tools that run nested
// agent loops (sub-agents, async workers) can stream their events back to the
// parent. The agent loop installs one automatically for every tool invocation;
// tools outside a loop may inject their own for testing.
func WithSubAgentEmitter(ctx context.Context, fn SubAgentEmitter) context.Context {
	return context.WithValue(ctx, subAgentEmitterKey{}, fn)
}

// SubAgentEmitterFromContext returns the emitter installed by the enclosing
// agent loop, or nil when the tool is running outside a loop. Callers should
// fall back to non-streaming behavior when nil is returned.
func SubAgentEmitterFromContext(ctx context.Context) SubAgentEmitter {
	fn, _ := ctx.Value(subAgentEmitterKey{}).(SubAgentEmitter)
	return fn
}

// DecorateForwardedEvent stamps Source and ParentID on an event that is being
// forwarded from a sub-agent to its parent stream. When the event already
// carries a Source (i.e. it came from a deeper nested agent), the new tag is
// prepended with ">" so the outermost hop appears first. ParentID is always
// overwritten with parentSessionKey — by the time the event reaches the top
// of the chain, it references the user-facing session.
func DecorateForwardedEvent(ev StreamEvent, source, parentSessionKey string) StreamEvent {
	if ev.Source == "" {
		ev.Source = source
	} else {
		ev.Source = source + ">" + ev.Source
	}
	ev.ParentID = parentSessionKey
	return ev
}
