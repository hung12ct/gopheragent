package llmfake

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

func TestScriptedProvider_PlaysTurnsInOrder(t *testing.T) {
	p := &ScriptedProvider{
		Turns: []Turn{
			{Content: "one"},
			{Content: "two"},
		},
	}
	for i, want := range []string{"one", "two"} {
		stream := make(chan agent.StreamEvent, 4)
		res, err := p.GenerateStream(context.Background(), nil, nil, stream)
		close(stream)
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if res.Content != want {
			t.Fatalf("turn %d content: want %q got %q", i, want, res.Content)
		}
	}
	if p.TurnsTaken() != 2 {
		t.Fatalf("expected TurnsTaken=2, got %d", p.TurnsTaken())
	}
}

func TestScriptedProvider_EmitsContentEvent(t *testing.T) {
	p := &ScriptedProvider{Turns: []Turn{{Content: "hello"}}}
	stream := make(chan agent.StreamEvent, 4)
	_, _ = p.GenerateStream(context.Background(), nil, nil, stream)
	close(stream)
	var got []agent.StreamEvent
	for ev := range stream {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 stream event, got %d", len(got))
	}
	cev, ok := got[0].Payload.(agent.ContentEvent)
	if !ok || cev.Text != "hello" {
		t.Fatalf("unexpected event payload: %+v", got[0].Payload)
	}
}

func TestScriptedProvider_ToolCallsTurn(t *testing.T) {
	p := &ScriptedProvider{Turns: []Turn{{
		ToolCalls: []agent.PendingToolCall{{ID: "c1", Name: "echo", ArgsJSON: `"hi"`}},
	}}}
	res, err := p.GenerateStream(context.Background(), nil, nil, make(chan agent.StreamEvent, 1))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "echo" {
		t.Fatalf("expected echo tool call, got %+v", res.ToolCalls)
	}
}

func TestScriptedProvider_ErrorTurn(t *testing.T) {
	sentinel := errors.New("synthetic")
	p := &ScriptedProvider{Turns: []Turn{{Err: sentinel}}}
	_, err := p.GenerateStream(context.Background(), nil, nil, make(chan agent.StreamEvent, 1))
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestScriptedProvider_FuncTurn(t *testing.T) {
	// Func turn inspects the inbound messages and branches.
	p := &ScriptedProvider{Turns: []Turn{{
		Func: func(_ context.Context, msgs []history.Message, _ *tools.Registry, _ chan<- agent.StreamEvent) (agent.LLMResult, error) {
			return agent.LLMResult{Content: "saw " + msgs[len(msgs)-1].Content}, nil
		},
	}}}
	res, err := p.GenerateStream(context.Background(), []history.Message{{Role: "user", Content: "ping"}}, nil, make(chan agent.StreamEvent, 1))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Content != "saw ping" {
		t.Fatalf("Func didn't see message: %q", res.Content)
	}
}

func TestScriptedProvider_ExhaustedDefault(t *testing.T) {
	p := &ScriptedProvider{Turns: []Turn{{Content: "one"}}}
	_, _ = p.GenerateStream(context.Background(), nil, nil, make(chan agent.StreamEvent, 1))
	// Second call: script exhausted, default kicks in ("done").
	stream := make(chan agent.StreamEvent, 4)
	res, err := p.GenerateStream(context.Background(), nil, nil, stream)
	close(stream)
	if err != nil {
		t.Fatalf("default turn errored: %v", err)
	}
	if res.Content != "done" {
		t.Fatalf("expected default 'done', got %q", res.Content)
	}
}

func TestScriptedProvider_ExhaustedStrictErrors(t *testing.T) {
	p := &ScriptedProvider{Turns: []Turn{{Content: "one"}}, Strict: true}
	_, _ = p.GenerateStream(context.Background(), nil, nil, make(chan agent.StreamEvent, 1))
	_, err := p.GenerateStream(context.Background(), nil, nil, make(chan agent.StreamEvent, 1))
	if err == nil {
		t.Fatal("expected error after strict script exhaustion")
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("expected exhaustion message, got %v", err)
	}
}

func TestScriptedProvider_Reset(t *testing.T) {
	p := &ScriptedProvider{Turns: []Turn{{Content: "x"}, {Content: "y"}}}
	_, _ = p.GenerateStream(context.Background(), nil, nil, make(chan agent.StreamEvent, 1))
	_, _ = p.GenerateStream(context.Background(), nil, nil, make(chan agent.StreamEvent, 1))
	p.Reset()
	stream := make(chan agent.StreamEvent, 1)
	res, _ := p.GenerateStream(context.Background(), nil, nil, stream)
	close(stream)
	if res.Content != "x" {
		t.Fatalf("Reset failed: expected 'x', got %q", res.Content)
	}
}

func TestScriptedProvider_ConcurrentCallsSerializeIndex(t *testing.T) {
	turns := make([]Turn, 50)
	for i := range turns {
		turns[i] = Turn{Content: "x"}
	}
	p := &ScriptedProvider{Turns: turns, Strict: true}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.GenerateStream(context.Background(), nil, nil, make(chan agent.StreamEvent, 1))
		}()
	}
	wg.Wait()
	if p.TurnsTaken() != 50 {
		t.Fatalf("expected 50 turns consumed under concurrency, got %d", p.TurnsTaken())
	}
}
