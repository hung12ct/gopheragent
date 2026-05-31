// Command memory_demo shows GopherAgent's cross-session memory in action.
//
// Two sessions run back-to-back against an in-process scripted LLM
// provider (no API key required). Session 1 contains a small exchange
// that the Consolidator turns into Notes; Session 2 starts fresh and
// the Loader injects those notes into the system prompt before the
// first LLM call — so the second session "remembers" the first.
//
// Run with:
//
//	go run ./examples/memory_demo
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/memory"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func main() {
	ctx := context.Background()

	store := memory.NewInMemStore()
	sm := history.NewInMemSessionManager("You are a helpful assistant.")
	reg := tools.NewRegistry()

	// Same user across both sessions. The MemoryScopeFn maps every
	// session for "alice" to a single memory scope so notes carry over.
	const aliceUserID = "alice"
	scopeFn := func(_ context.Context, _ string) string {
		return "user:" + aliceUserID
	}

	provider := newScriptedProvider()

	consolidator := &agent.Consolidator{
		Store: store,
		LLM:   provider,
		// Lower the threshold for the demo so 4-message sessions consolidate.
		MinTranscriptMessages: 2,
		MaxNotes:              4,
	}

	loop := agent.New(sm, reg, provider,
		agent.WithMemory(store, agent.MemoryConfig{TokenBudget: 500, MaxNotes: 30}),
		agent.WithMemoryScope(scopeFn),
		agent.WithMemoryConsolidator(consolidator),
	)

	fmt.Println("=== Session 1 (Monday morning) ===")
	provider.queueDialogue(scriptForSession1)
	resp1, err := loop.RunIteration(ctx, "session-monday", "summarize my open Jira tickets in BACKEND")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("assistant:", resp1)

	// The Consolidator fires in a detached goroutine after Run returns.
	// In a real app you would not block on it; for the demo we wait
	// until at least one note lands so the next session sees something.
	waitForNote(ctx, store, "user:"+aliceUserID)
	dumpNotes(ctx, store, "user:"+aliceUserID)

	fmt.Println("\n=== Session 2 (Tuesday morning) ===")
	provider.queueDialogue(scriptForSession2)
	resp2, err := loop.RunIteration(ctx, "session-tuesday", "summarize my open tickets")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("assistant:", resp2)
	if !provider.session2SawNotes {
		fmt.Println("\n(note: the scripted provider did not observe the memory block — wiring bug)")
	} else {
		fmt.Println("\n(memory block was injected into session 2's system prompt)")
	}
}

// --- scripted LLM provider ---

// scriptedProvider is a deterministic stand-in for a real LLMProvider.
// Each Generate call pops one queued response. A separate consolidator
// response is returned when the system prompt looks like the
// Consolidator's extraction prompt; that lets the demo run end-to-end
// against one provider instance without a real API.
type scriptedProvider struct {
	dialogue         []string
	session2SawNotes bool
}

func newScriptedProvider() *scriptedProvider { return &scriptedProvider{} }

func (p *scriptedProvider) queueDialogue(turns []string) {
	p.dialogue = append(p.dialogue, turns...)
}

func (p *scriptedProvider) GenerateStream(_ context.Context, msgs []history.Message, _ *tools.Registry, ch chan<- agent.StreamEvent) (agent.LLMResult, error) {
	// Detect the consolidator's structured-output request. Match a
	// distinctive phrase from the merge-aware prompt so future prompt
	// rewordings don't silently break the demo.
	if len(msgs) > 0 && strings.Contains(msgs[0].Content, "memory consolidator") {
		return agent.LLMResult{Content: consolidatorReply}, nil
	}

	// For non-consolidator calls, inspect the system message to check
	// whether session 2 received the memory block.
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "## Long-term memory") {
			p.session2SawNotes = true
			break
		}
	}

	var reply string
	if len(p.dialogue) > 0 {
		reply = p.dialogue[0]
		p.dialogue = p.dialogue[1:]
	} else {
		reply = "ok"
	}
	ch <- agent.Event(agent.ContentEvent{Text: reply})
	return agent.LLMResult{Content: reply}, nil
}

// --- canned data ---

var scriptForSession1 = []string{
	"Sure — which Jira workspace? I can see acme-corp and beta-co.",
}

var scriptForSession2 = []string{
	"Pulling open tickets in BACKEND for acme-corp, grouped by assignee.",
}

const consolidatorReply = `{"notes":[
  {"key":"jira.default_workspace","content":"User's default Jira workspace is acme-corp","tags":["preference"]},
  {"key":"jira.frequent_project","content":"User frequently asks about the BACKEND project","tags":["preference"]},
  {"key":"prefs.ticket_grouping","content":"User prefers ticket summaries grouped by assignee","tags":["preference"]}
]}`

// --- helpers ---

// waitForNote spins until the store has at least one note for scope. The
// consolidator runs in a detached goroutine that's normally fire-and-
// forget; for a demo we want determinism, so we poll briefly.
func waitForNote(ctx context.Context, store memory.Store, scope string) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		notes, _ := store.List(ctx, scope, memory.ListOpts{})
		if len(notes) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// dumpNotes prints the current notes for scope so the demo output makes
// the cross-session knowledge transfer visible.
func dumpNotes(ctx context.Context, store memory.Store, scope string) {
	notes, _ := store.List(ctx, scope, memory.ListOpts{})
	if len(notes) == 0 {
		fmt.Println("  (no notes — consolidator did not run)")
		return
	}
	fmt.Println("  notes after session 1:")
	for _, n := range notes {
		fmt.Printf("    - [%s] %s\n", n.Key, n.Content)
	}
}
