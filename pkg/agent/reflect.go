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
