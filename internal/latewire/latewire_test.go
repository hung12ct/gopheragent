package latewire

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

type answerLLM struct{}

func (answerLLM) GenerateStream(context.Context, []history.Message, *tools.Registry, chan<- agent.StreamEvent) (agent.LLMResult, error) {
	return agent.LLMResult{Content: "ok"}, nil
}

type namedTool string

func (n namedTool) Descriptor() tools.ToolDescriptor { return tools.ToolDescriptor{Name: string(n)} }
func (namedTool) Execute(context.Context, string) (tools.Result, error) {
	return tools.Text("ok"), nil
}

// captureLog runs fn and returns what it logged.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	fn()
	return buf.String()
}

func newLoop(toolNames ...string) *agent.AgentLoop {
	reg := tools.NewRegistry()
	for _, n := range toolNames {
		reg.Register(namedTool(n))
	}
	return agent.New(history.NewInMemSessionManager("sys"), reg, answerLLM{})
}

func TestWatch_WarnsOnceAboutLateAttachments(t *testing.T) {
	loop := newLoop("early")
	Watch(loop, "auditlog", true)
	loop.Tools.Register(namedTool("late"))
	loop.ConfirmHITL = func(context.Context, string, string) bool { return true }

	out := captureLog(t, func() {
		_, _ = loop.RunIteration(context.Background(), "s", "a")
		_, _ = loop.RunIteration(context.Background(), "s", "b")
	})
	if !strings.Contains(out, "tool late") || !strings.Contains(out, "ConfirmHITL") || strings.Contains(out, "tool early") {
		t.Fatalf("warning = %q, want it to name the late tool and ConfirmHITL only", out)
	}
	if strings.Count(out, "attached after Instrument") != 1 {
		t.Fatalf("warned %d times, want once", strings.Count(out, "attached after Instrument"))
	}
}

func TestWatch_SilentWhenInstrumentIsLast(t *testing.T) {
	loop := newLoop("a", "b")
	loop.ConfirmHITL = func(context.Context, string, string) bool { return true }
	Watch(loop, "auditlog", true)
	loop.Tools.Use(func(next tools.Tool) tools.Tool { return next }) // more wrapping is not "late"
	if out := captureLog(t, func() { _, _ = loop.RunIteration(context.Background(), "s", "q") }); out != "" {
		t.Fatalf("unexpected warning: %q", out)
	}
}
