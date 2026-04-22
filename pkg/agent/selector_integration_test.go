package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/hung12ct/gopheragent/pkg/history"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

// recordingProvider captures the tool names visible to the LLM on each call,
// returning a canned final-answer response so the loop terminates cleanly.
type recordingProvider struct {
	seenToolNames [][]string
	reply         string
}

func (p *recordingProvider) GenerateStream(_ context.Context, _ []history.Message, available *tools.Registry, ch chan<- StreamEvent) (LLMResult, error) {
	names := make([]string, 0)
	for _, t := range available.GetAll() {
		names = append(names, t.Name())
	}
	p.seenToolNames = append(p.seenToolNames, names)
	ch <- StreamEvent{Type: "content", Content: p.reply}
	return LLMResult{Content: p.reply}, nil
}

type keywordEmbed struct{}

func (keywordEmbed) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		t = strings.ToLower(t)
		var v [2]float32
		if strings.Contains(t, "weather") || strings.Contains(t, "forecast") {
			v[0] = 1
		}
		if strings.Contains(t, "order") || strings.Contains(t, "inventory") {
			v[1] = 1
		}
		out[i] = v[:]
	}
	return out, nil
}

func TestAgentLoop_ToolSelector_FiltersPresentedTools(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "get_weather"})
	reg.Register(&echoTool{name: "lookup_order"})
	reg.Register(&echoTool{name: "unrelated_tool"})

	// Override descriptions so the keyword embedder can discriminate.
	descReg := tools.NewRegistry()
	descReg.Register(&descTool{name: "get_weather", desc: "returns the weather forecast"})
	descReg.Register(&descTool{name: "lookup_order", desc: "fetches order details from inventory"})
	descReg.Register(&descTool{name: "unrelated_tool", desc: "does nothing in particular"})

	sel, err := tools.NewSelector(context.Background(), descReg, keywordEmbed{}, 1)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}

	provider := &recordingProvider{reply: "ok"}
	sm := history.NewInMemSessionManager("sys")
	loop := NewAgentLoop(sm, descReg, provider)
	loop.ToolSelector = sel

	if _, err := loop.RunIteration(context.Background(), "s1", "what's the weather today?"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if len(provider.seenToolNames) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(provider.seenToolNames))
	}
	seen := provider.seenToolNames[0]
	if len(seen) != 1 || seen[0] != "get_weather" {
		t.Fatalf("expected LLM to see only [get_weather], got %v", seen)
	}
}

func TestAgentLoop_NoSelector_SeesAllTools(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&echoTool{name: "a"})
	reg.Register(&echoTool{name: "b"})
	reg.Register(&echoTool{name: "c"})

	provider := &recordingProvider{reply: "ok"}
	sm := history.NewInMemSessionManager("sys")
	loop := NewAgentLoop(sm, reg, provider)
	// ToolSelector nil by default.

	if _, err := loop.RunIteration(context.Background(), "s1", "hi"); err != nil {
		t.Fatalf("RunIteration: %v", err)
	}
	if len(provider.seenToolNames[0]) != 3 {
		t.Fatalf("expected all 3 tools without selector, got %v", provider.seenToolNames[0])
	}
}

type descTool struct {
	name string
	desc string
}

func (t *descTool) Name() string                                        { return t.name }
func (t *descTool) Description() string                                 { return t.desc }
func (t *descTool) ParametersSchema() tools.ToolSchema                  { return tools.ToolSchema{} }
func (t *descTool) RequiresConfirmation() bool                          { return false }
func (t *descTool) Display() tools.ToolDisplay { return tools.DefaultDisplay(t.Name(), t.Description()) }
func (t *descTool) Execute(_ context.Context, _ string) (string, error) { return "", nil }
