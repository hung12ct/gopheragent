package eval

import (
	"context"
	"iter"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
)

// Target is what a trial runs against. *agent.AgentLoop already satisfies
// the Run signature; use WrapLoop to adapt one. The interface is defined
// consumer-side (here) so eval never has to import a concrete loop type.
type Target interface {
	// Run streams the agent's events for one turn. The eval harness ranges
	// the iterator to completion to build a Transcript.
	Run(ctx context.Context, sessionKey string, msg history.Message) iter.Seq[agent.StreamEvent]
}

// HistoryReader is an optional Target extension. Targets that implement it
// get tool results joined into the Transcript after each turn — tool results
// are not stream events, they live in session history as Role="tool"
// messages. Targets without it still capture tool calls, just with empty
// ToolCallRecord.Result.
type HistoryReader interface {
	History(ctx context.Context, sessionKey string) ([]history.Message, error)
}

// TargetFactory builds a fresh, isolated Target for one trial. It is called
// once per (task, trial); implementations must not share mutable state
// (scripted providers, in-memory sessions) across calls unless that state is
// safe to reuse — otherwise trials leak history and scripted turns into each
// other. taskID and trial are passed so a factory can, e.g., pick a scripted
// provider per task.
type TargetFactory func(ctx context.Context, taskID string, trial int) (Target, error)

// loopTarget adapts an *agent.AgentLoop to Target + HistoryReader.
type loopTarget struct{ loop *agent.AgentLoop }

func (t loopTarget) Run(ctx context.Context, sessionKey string, msg history.Message) iter.Seq[agent.StreamEvent] {
	return t.loop.Run(ctx, sessionKey, msg)
}

func (t loopTarget) History(ctx context.Context, sessionKey string) ([]history.Message, error) {
	return t.loop.Sessions.History(ctx, sessionKey)
}

// WrapLoop adapts an AgentLoop into a Target that also implements
// HistoryReader (via loop.Sessions.History), so captured transcripts carry
// tool results. This is the one-liner most adopters want inside a
// TargetFactory.
func WrapLoop(loop *agent.AgentLoop) Target {
	return loopTarget{loop: loop}
}
