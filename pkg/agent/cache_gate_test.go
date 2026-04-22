package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/cache"
	"github.com/hung12ct/gopheragent/pkg/tools"
)

type gateTool struct {
	name      string
	cacheable bool
	hasFlag   bool
	calls     atomic.Int32
}

func (g *gateTool) Name() string                       { return g.name }
func (g *gateTool) Description() string                { return "gate tool" }
func (g *gateTool) ParametersSchema() tools.ToolSchema { return tools.ToolSchema{} }
func (g *gateTool) RequiresConfirmation() bool         { return false }
func (g *gateTool) Display() tools.ToolDisplay { return tools.DefaultDisplay(g.Name(), g.Description()) }
func (g *gateTool) Execute(_ context.Context, args string) (string, error) {
	g.calls.Add(1)
	return "gate:" + args, nil
}

// cacheableGateTool is a gateTool that also implements tools.Cacheable.
type cacheableGateTool struct{ gateTool }

func (c *cacheableGateTool) Cacheable() bool { return c.gateTool.cacheable }

func runTwice(t *testing.T, loop *AgentLoop) {
	t.Helper()
	for i := 0; i < 2; i++ {
		if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
}

func twoCallScript() *scriptProvider {
	return &scriptProvider{turns: []LLMResult{
		{ToolCalls: []PendingToolCall{{ID: "c1", Name: "gate", ArgsJSON: `{"q":"x"}`}}},
		{Content: "final-1"},
		{ToolCalls: []PendingToolCall{{ID: "c2", Name: "gate", ArgsJSON: `{"q":"x"}`}}},
		{Content: "final-2"},
	}}
}

func TestCacheGate_NoCacheable_Bypasses(t *testing.T) {
	g := &gateTool{name: "gate"}
	loop, _ := setup(twoCallScript(), g)
	loop.Cache = cache.NewSearchCache(10, time.Minute)

	runTwice(t, loop)

	if n := g.calls.Load(); n != 2 {
		t.Fatalf("non-Cacheable tool must bypass cache: expected 2 execs, got %d", n)
	}
}

func TestCacheGate_CacheableFalse_Bypasses(t *testing.T) {
	g := &cacheableGateTool{gateTool: gateTool{name: "gate", cacheable: false}}
	loop, _ := setup(twoCallScript(), g)
	loop.Cache = cache.NewSearchCache(10, time.Minute)

	runTwice(t, loop)

	if n := g.calls.Load(); n != 2 {
		t.Fatalf("Cacheable()==false must bypass cache: expected 2 execs, got %d", n)
	}
}

func TestCacheGate_CacheableTrue_Caches(t *testing.T) {
	g := &cacheableGateTool{gateTool: gateTool{name: "gate", cacheable: true}}
	loop, _ := setup(twoCallScript(), g)
	loop.Cache = cache.NewSearchCache(10, time.Minute)

	var hit int
	loop.OnEvent(func(_ context.Context, _ string, ev StreamEvent) {
		if ev.Type == "thought" && strings.Contains(ev.Content, "Cache hit for gate") {
			hit++
		}
	})

	runTwice(t, loop)

	if n := g.calls.Load(); n != 1 {
		t.Fatalf("Cacheable()==true must cache: expected 1 exec, got %d", n)
	}
	if hit != 1 {
		t.Fatalf("expected 1 cache-hit thought event, got %d", hit)
	}
}

func TestCacheGate_NilCache_NoOp(t *testing.T) {
	g := &cacheableGateTool{gateTool: gateTool{name: "gate", cacheable: true}}
	loop, _ := setup(twoCallScript(), g)
	// loop.Cache remains nil

	runTwice(t, loop)

	if n := g.calls.Load(); n != 2 {
		t.Fatalf("nil cache must not affect execution: expected 2 execs, got %d", n)
	}
}
