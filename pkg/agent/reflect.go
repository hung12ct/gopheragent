package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/hung12ct/gopheragent/pkg/history"
)

// defaultReflectPrompt is the critique instruction appended to the conversation
// at each self-critique round when ReflectPrompt is empty. It is intentionally
// domain-agnostic: it asks for a verbatim repeat on a correct answer so a
// no-change round produces identical output and the loop can detect
// convergence early.
const defaultReflectPrompt = "Review your previous answer against the user's original question. " +
	"If it is incomplete, incorrect, or can be materially improved, produce the revised answer and nothing else. " +
	"If the current answer is already correct, repeat it verbatim — no commentary, no preface, no mention of this review. " +
	"Output only the final answer text."

// runReflectRounds drives the configured self-critique passes and returns
// the answer to keep. msgs must already end with the assistant's current
// answer; the last message's Content is rewritten in place for each round
// so the next critique sees the answer it is reviewing, and restored when
// a round is rejected.
//
// Without a Scorer this is last-wins: any non-empty, textually different
// revision is adopted, which is what the loop has always done. With a
// Scorer it is best-wins: a revision must strictly beat the best score so
// far, so ties and regressions keep the earlier answer and a critique
// pass can no longer degrade the response.
func (al *AgentLoop) runReflectRounds(ctx context.Context, st *iterationState, msgs []history.Message, finalContent string) string {
	best := finalContent
	last := len(msgs) - 1
	bestScore, haveBest := al.scoreCandidate(ctx, st, msgs, best, 0)

	for r := 1; r <= al.Reflect; r++ {
		al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{
			Message: fmt.Sprintf("Self-critique pass %d/%d...", r, al.Reflect),
		}))
		revised, rerr := al.reflectOnce(ctx, st.sessionKey, msgs, r, st.streamChan)
		if rerr != nil {
			al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{
				Message: fmt.Sprintf("Self-critique aborted: %v", rerr),
			}))
			break
		}
		if revised == "" || revised == best {
			continue
		}

		msgs[last].Content = revised
		var adopted float64
		if al.Scorer != nil {
			score, ok := al.scoreCandidate(ctx, st, msgs, revised, r)
			// An unranked revision cannot be shown to be an improvement,
			// so it loses to the incumbent the same way a lower-scoring
			// one does.
			if !ok || (haveBest && score <= bestScore) {
				msgs[last].Content = best
				al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{
					Message: fmt.Sprintf("Self-critique pass %d discarded: %s.", r, rejectionDetail(score, ok, bestScore)),
				}))
				continue
			}
			bestScore, haveBest, adopted = score, true, score
		}

		best = revised
		al.emit(ctx, st.sessionKey, st.streamChan, Event(ReflectedEvent{
			Text:  best,
			Round: r,
			Score: adopted,
		}))
	}
	return best
}

// scoreCandidate ranks one candidate answer. Reports ok=false when no
// Scorer is configured or the Scorer failed — a scorer error degrades the
// round to unranked rather than failing the turn.
func (al *AgentLoop) scoreCandidate(ctx context.Context, st *iterationState, msgs []history.Message, answer string, round int) (float64, bool) {
	if al.Scorer == nil {
		return 0, false
	}
	score, err := al.Scorer.Score(ctx, RunResult{Answer: answer, Messages: msgs, Round: round})
	if err != nil {
		al.emit(ctx, st.sessionKey, st.streamChan, Event(ThoughtEvent{
			Message: fmt.Sprintf("Scorer failed on round %d: %v", round, err),
		}))
		return 0, false
	}
	return score, true
}

// rejectionDetail renders why a round lost, for the thought event. A
// round only reaches here unranked or beaten by an existing best, so
// those are the only two cases.
func rejectionDetail(score float64, scored bool, best float64) string {
	if !scored {
		return "the scorer could not rank it"
	}
	return fmt.Sprintf("scored %.4g, not better than the kept %.4g", score, best)
}

// reflectOnce runs a single self-critique round against the conversation in
// baseMsgs (which must already include the assistant's current answer).
//
// The synthetic critique user message is NOT persisted; it lives only for the
// duration of this call so prompt cost scales linearly with rounds instead of
// ballooning the saved history. Reflection is always text-only — the tool
// registry passed to the provider is nil, so even if the model were inclined
// to call a tool it has nothing to call.
//
// Content events emitted by the provider are forwarded to streamChan tagged
// with Source="reflect:<round>" so streaming UIs can render / filter critique
// passes distinctly from the canonical answer. The final accumulated text is
// returned to the caller, which is responsible for emitting the canonical
// ReflectedEvent and deciding whether to update history.
func (al *AgentLoop) reflectOnce(
	ctx context.Context,
	sessionKey string,
	baseMsgs []history.Message,
	round int,
	streamChan chan<- StreamEvent,
) (string, error) {
	prompt := al.ReflectPrompt
	if prompt == "" {
		prompt = defaultReflectPrompt
	}

	critiqueMsgs := make([]history.Message, 0, len(baseMsgs)+1)
	critiqueMsgs = append(critiqueMsgs, baseMsgs...)
	critiqueMsgs = append(critiqueMsgs, history.Message{Role: "user", Content: prompt})

	source := fmt.Sprintf("reflect:%d", round)

	pChan := make(chan StreamEvent, 32)
	done := make(chan struct{})
	var buf strings.Builder
	go func() {
		defer close(done)
		for ev := range pChan {
			if p, ok := ev.Payload.(ContentEvent); ok {
				buf.WriteString(p.Text)
			}
			// Don't forward usage/done from the nested call — the outer loop
			// owns the "done" lifecycle; nested usage can be confusing when
			// reported multiple times per iteration.
			if ev.Type == EventTypeUsage || ev.Type == EventTypeDone {
				continue
			}
			tagged := ev
			if tagged.Source == "" {
				tagged.Source = source
			} else {
				tagged.Source = source + ">" + tagged.Source
			}
			select {
			case streamChan <- tagged:
				for _, h := range al.EventHandlers {
					h(ctx, sessionKey, tagged)
				}
			case <-ctx.Done():
				for range pChan {
				}
				return
			}
		}
	}()

	_, err := al.LLM.GenerateStream(ctx, critiqueMsgs, nil, pChan)
	close(pChan)
	<-done

	if err != nil {
		return "", fmt.Errorf("agent: reflect round %d: %w", round, err)
	}
	return strings.TrimSpace(buf.String()), nil
}
