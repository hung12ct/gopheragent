package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// deferredLoop builds a DeferApprovals loop over sm whose ConfirmHITL fails
// the test: a deferred gate must never block on it.
func deferredLoop(t *testing.T, sm *history.InMemSessionManager, provider LLMProvider, toolList ...tools.Tool) *AgentLoop {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tl := range toolList {
		reg.Register(tl)
	}
	loop := NewAgentLoop(sm, reg, provider)
	loop.DeferApprovals = true
	loop.ConfirmHITL = func(context.Context, string, string) bool {
		t.Error("ConfirmHITL called under DeferApprovals")
		return false
	}
	return loop
}

// drain collects a run's ActionRequiredEvent IDs and its terminal error.
func drain(seq iter.Seq[StreamEvent]) (ids []string, lastErr error) {
	for ev := range seq {
		switch p := ev.Payload.(type) {
		case ActionRequiredEvent:
			ids = append(ids, p.ID)
		case ErrorEvent:
			lastErr = p.Err
		}
	}
	return ids, lastErr
}

func toolTail(t *testing.T, sm *history.InMemSessionManager) []history.Message {
	t.Helper()
	msgs, err := sm.History(context.Background(), "s1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	return msgs
}

func TestDeferApprovals_SuspendThenResumeAfterRestart(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	wipe := &sideEffectTool{name: "wipe", confirm: true}
	read := &sideEffectTool{name: "read"}
	first := deferredLoop(t, sm, &scriptProvider{turns: []LLMResult{{ToolCalls: []PendingToolCall{
		{ID: "c1", Name: "wipe", ArgsJSON: `{}`},
		{ID: "c2", Name: "read", ArgsJSON: `{}`},
	}}}}, wipe, read)

	ids, err := drain(first.RunText(context.Background(), "s1", "wipe it"))
	if !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("terminal error = %v, want ErrApprovalPending", err)
	}
	if len(ids) != 1 || ids[0] != "c1" {
		t.Fatalf("ActionRequired IDs = %v, want [c1]", ids)
	}
	if wipe.runs.Load() != 0 || read.runs.Load() != 1 {
		t.Fatalf("runs wipe=%d read=%d, want 0 and 1", wipe.runs.Load(), read.runs.Load())
	}

	// A fresh loop over the same store stands in for a restarted process.
	second := deferredLoop(t, sm, &scriptProvider{turns: []LLMResult{{Content: "wiped"}}}, wipe, read)
	pending, err := second.PendingApprovals(context.Background(), "s1")
	if err != nil || len(pending) != 1 || pending[0].ID != "c1" {
		t.Fatalf("PendingApprovals = %v, %v; want [c1]", pending, err)
	}
	seq, err := second.Resume(context.Background(), "s1", map[string]bool{"c1": true})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := drain(seq); err != nil {
		t.Fatalf("resumed run error: %v", err)
	}
	if wipe.runs.Load() != 1 || read.runs.Load() != 1 {
		t.Fatalf("runs wipe=%d read=%d, want 1 and 1", wipe.runs.Load(), read.runs.Load())
	}

	msgs := toolTail(t, sm)
	n := len(msgs)
	if msgs[n-3].ToolCallID != "c1" || msgs[n-2].ToolCallID != "c2" || msgs[n-1].Content != "wiped" {
		t.Fatalf("tail = %+v, want c1 result, c2 result, final answer", msgs[n-3:])
	}
	if _, err := second.Resume(context.Background(), "s1", nil); !errors.Is(err, ErrNothingToResume) {
		t.Fatalf("second Resume error = %v, want ErrNothingToResume", err)
	}
}

func TestDeferApprovals_ResumeDenyAndUnknownID(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	wipe := &sideEffectTool{name: "wipe", confirm: true}
	loop := deferredLoop(t, sm, &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "wipe", ArgsJSON: `{}`}}},
		{Content: "ok, not wiping"},
	}}, wipe)
	if _, err := drain(loop.RunText(context.Background(), "s1", "wipe it")); !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("terminal error = %v, want ErrApprovalPending", err)
	}

	if _, err := loop.Resume(context.Background(), "s1", map[string]bool{"nope": true}); !errors.Is(err, ErrUnknownApproval) {
		t.Fatalf("unknown ID error = %v, want ErrUnknownApproval", err)
	}
	seq, err := loop.Resume(context.Background(), "s1", map[string]bool{"c1": false})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := drain(seq); err != nil {
		t.Fatalf("resumed run error: %v", err)
	}
	if wipe.runs.Load() != 0 {
		t.Fatalf("denied tool ran %d times", wipe.runs.Load())
	}
	msgs := toolTail(t, sm)
	reply := msgs[len(msgs)-2]
	if reply.ToolCallID != "c1" || !reply.IsError || !strings.Contains(reply.Content, "permission gate") {
		t.Fatalf("denial reply = %+v", reply)
	}
}

func TestDeferApprovals_DependentWaveWaitsForResume(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	wipe := &sideEffectTool{name: "wipe", confirm: true}
	read := &sideEffectTool{name: "read"}
	loop := deferredLoop(t, sm, &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{
			{ID: "c1", Name: "wipe", ArgsJSON: `{}`},
			{ID: "c2", Name: "read", ArgsJSON: `{"after":"<output_of:c1>"}`},
		}},
		{Content: "done"},
	}}, wipe, read)
	if _, err := drain(loop.RunText(context.Background(), "s1", "go")); !errors.Is(err, ErrApprovalPending) {
		t.Fatalf("terminal error = %v, want ErrApprovalPending", err)
	}
	if read.runs.Load() != 0 {
		t.Fatalf("dependent call ran before its input was approved")
	}
	// c2 is pending but ungated: a decision for it would be ignored, so it
	// must be rejected rather than let the call run "denied".
	if _, err := loop.Resume(context.Background(), "s1", map[string]bool{"c2": false}); !errors.Is(err, ErrUnknownApproval) {
		t.Fatalf("decision for ungated call: err = %v, want ErrUnknownApproval", err)
	}

	seq, err := loop.Resume(context.Background(), "s1", map[string]bool{"c1": true})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := drain(seq); err != nil {
		t.Fatalf("resumed run error: %v", err)
	}
	msgs := toolTail(t, sm)
	c2 := msgs[len(msgs)-2]
	if c2.ToolCallID != "c2" || !strings.Contains(c2.Content, `ran:{}`) {
		t.Fatalf("c2 reply = %+v, want args substituted from c1", c2)
	}
}

func TestDeferApprovals_PartialDecisionsSuspendAgain(t *testing.T) {
	sm := history.NewInMemSessionManager("sys")
	wipe := &sideEffectTool{name: "wipe", confirm: true}
	loop := deferredLoop(t, sm, &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{
			{ID: "c1", Name: "wipe", ArgsJSON: `{"k":1}`},
			{ID: "c2", Name: "wipe", ArgsJSON: `{"k":2}`},
		}},
	}}, wipe)
	if ids, err := drain(loop.RunText(context.Background(), "s1", "go")); !errors.Is(err, ErrApprovalPending) || len(ids) != 2 {
		t.Fatalf("first run: ids=%v err=%v", ids, err)
	}

	seq, err := loop.Resume(context.Background(), "s1", map[string]bool{"c1": true})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	ids, err := drain(seq)
	if !errors.Is(err, ErrApprovalPending) || len(ids) != 1 || ids[0] != "c2" {
		t.Fatalf("partial resume: ids=%v err=%v, want [c2] and ErrApprovalPending", ids, err)
	}
	if wipe.runs.Load() != 1 {
		t.Fatalf("wipe ran %d times, want 1", wipe.runs.Load())
	}
	pending, _ := loop.PendingApprovals(context.Background(), "s1")
	if len(pending) != 1 || pending[0].ID != "c2" {
		t.Fatalf("PendingApprovals = %v, want [c2]", pending)
	}
}
