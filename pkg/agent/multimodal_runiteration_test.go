package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// partsCapturingProviderRegistry records the slice of messages handed to it
// on the most recent call so tests can assert that user-supplied Parts
// flow into the LLM prompt.
type partsCapturingProviderRegistry struct {
	mu     sync.Mutex
	last   []history.Message
	called chan struct{}
}

func (p *partsCapturingProviderRegistry) GenerateStream(_ context.Context, msgs []history.Message, _ *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	p.mu.Lock()
	cp := make([]history.Message, len(msgs))
	copy(cp, msgs)
	p.last = cp
	p.mu.Unlock()
	if p.called != nil {
		select {
		case p.called <- struct{}{}:
		default:
		}
	}
	ch <- Event(ContentEvent{Text: "ok"})
	return LLMResult{Content: "ok"}, nil
}

func TestRunIterationStreamMessage_FlowsPartsIntoLLMCall(t *testing.T) {
	prov := &partsCapturingProviderRegistry{called: make(chan struct{}, 1)}
	loop, _ := setup(prov)

	msg := history.Message{
		Role:    "user",
		Content: "describe this image",
		Parts: []history.MediaPart{
			history.NewTextPart("describe this image"),
			history.NewImagePartURL("image/png", "https://example.test/cat.png"),
		},
	}

	ch := make(chan StreamEvent, 16)
	go loop.RunIterationStreamMessage(context.Background(), "s1", msg, ch)
	for range ch {
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	var userMsg *history.Message
	for i := range prov.last {
		if prov.last[i].Role == "user" {
			userMsg = &prov.last[i]
			break
		}
	}
	if userMsg == nil {
		t.Fatal("no user message reached the LLM")
	}
	if len(userMsg.Parts) != 2 {
		t.Fatalf("expected 2 parts in the user message, got %d (parts=%+v)", len(userMsg.Parts), userMsg.Parts)
	}
	if userMsg.Parts[1].Type != history.PartImage {
		t.Fatalf("expected image part at index 1, got %+v", userMsg.Parts[1])
	}
}

func TestRunIterationMessage_DefaultsRoleToUser(t *testing.T) {
	prov := &partsCapturingProviderRegistry{called: make(chan struct{}, 1)}
	loop, sm := setup(prov)

	if _, err := loop.RunIterationMessage(context.Background(), "s1", history.Message{Content: "hello"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stored, _ := sm.History(context.Background(), "s1")
	var saw bool
	for _, m := range stored {
		if m.Role == "user" && m.Content == "hello" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("user message not stored with default role: history=%+v", stored)
	}
}

func TestRunIteration_StringEntrypointStillWorks(t *testing.T) {
	// The pre-existing string-based RunIteration path must continue to
	// work after the multimodal refactor.
	prov := &partsCapturingProviderRegistry{}
	loop, _ := setup(prov)

	got, err := loop.RunIteration(context.Background(), "s1", "plain text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ok" {
		t.Fatalf("got %q, want %q", got, "ok")
	}
}
