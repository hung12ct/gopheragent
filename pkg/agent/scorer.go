package agent

import (
	"context"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// RunResult is one candidate answer handed to a Scorer for ranking. It is
// deliberately narrow: everything a scorer needs to judge a candidate,
// nothing that ties it to the axis the candidate came from.
//
// Answer is the candidate final-answer text. Messages is the conversation
// it terminates, including the candidate itself as the last assistant
// message — read-only; a scorer must not mutate it. Round is the
// 1-indexed refinement pass for sequential scoring (self-critique), and 0
// for the model's original, unrevised answer.
type RunResult struct {
	Answer   string
	Messages []history.Message
	Round    int
}

// Scorer ranks candidate answers so the loop can keep the best one rather
// than the last one. Higher scores win; the unit is the implementation's
// business (a 0–100 rubric, a compile-pass count, a negated latency).
//
// Score is called once per candidate on the loop goroutine, so a slow
// implementation adds directly to turn latency. It receives the turn's
// ctx and must honor cancellation. Returning an error drops that
// candidate from consideration without failing the turn — the loop keeps
// the best-scoring candidate it did manage to rank.
//
// Implementations that call an LLM to judge multiply the turn's token
// spend; that spend is invisible to BudgetTracker unless the scorer
// itself accounts for it.
type Scorer interface {
	Score(ctx context.Context, r RunResult) (float64, error)
}

// ScorerFunc adapts a plain function to Scorer.
type ScorerFunc func(ctx context.Context, r RunResult) (float64, error)

// Score implements Scorer.
func (f ScorerFunc) Score(ctx context.Context, r RunResult) (float64, error) {
	return f(ctx, r)
}
