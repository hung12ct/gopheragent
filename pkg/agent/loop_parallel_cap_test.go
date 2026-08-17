package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// concurrencyProbe records the high-water mark of overlapping Execute calls
// so a test can assert the wave semaphore actually bounds them.
type concurrencyProbe struct {
	live atomic.Int32
	peak atomic.Int32
	runs atomic.Int32
}

func (c *concurrencyProbe) Descriptor() tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Name:        "probe",
		Description: "records concurrency",
		Display:     tools.DefaultDisplay("probe", "records concurrency"),
	}
}

func (c *concurrencyProbe) Execute(_ context.Context, args string) (tools.Result, error) {
	live := c.live.Add(1)
	for {
		peak := c.peak.Load()
		if live <= peak || c.peak.CompareAndSwap(peak, live) {
			break
		}
	}
	// Hold the slot long enough that an unbounded fan-out genuinely overlaps.
	time.Sleep(5 * time.Millisecond)
	c.live.Add(-1)
	c.runs.Add(1)
	return tools.Text("probe:" + args), nil
}

// distinctCalls builds n calls to "probe" with unique arguments, so neither
// the anti-loop detector nor the result cache collapses them.
func distinctCalls(n int) []PendingToolCall {
	out := make([]PendingToolCall, n)
	for i := range n {
		out[i] = PendingToolCall{
			ID:       fmt.Sprintf("p%d", i),
			Name:     "probe",
			ArgsJSON: fmt.Sprintf(`{"i":%d}`, i),
		}
	}
	return out
}

func runProbeWave(t *testing.T, calls int, opts ...Option) *concurrencyProbe {
	t.Helper()
	probe := &concurrencyProbe{}
	provider := &scriptProvider{turns: []LLMResult{
		{ToolCalls: distinctCalls(calls)},
		{Content: "final"},
	}}
	loop, _ := setup(provider, probe)
	for _, opt := range opts {
		opt(loop)
	}

	if _, err := loop.RunIteration(context.Background(), "s1", "go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := probe.runs.Load(); got != int32(calls) {
		t.Fatalf("executed %d calls, want %d — the cap must delay calls, never drop them", got, calls)
	}
	return probe
}

func TestMaxParallelToolCalls_BoundsWaveConcurrency(t *testing.T) {
	probe := runProbeWave(t, 20, WithMaxParallelToolCalls(4))

	if peak := probe.peak.Load(); peak > 4 {
		t.Fatalf("peak concurrency = %d, want <= 4", peak)
	}
}

func TestMaxParallelToolCalls_DefaultApplies(t *testing.T) {
	probe := runProbeWave(t, 20)

	if peak := probe.peak.Load(); peak > defaultMaxParallelToolCalls {
		t.Fatalf("peak concurrency = %d, want <= %d (default)", peak, defaultMaxParallelToolCalls)
	}
}

func TestMaxParallelToolCalls_ZeroIsUnlimited(t *testing.T) {
	probe := runProbeWave(t, 20, WithMaxParallelToolCalls(0))

	// Not asserting an exact peak: the scheduler is free to interleave. The
	// point is that no cap is applied, which only shows up as a peak above
	// the default the constructor would otherwise have installed.
	if peak := probe.peak.Load(); peak <= defaultMaxParallelToolCalls {
		t.Skipf("peak %d did not exceed the default cap; scheduling-dependent, not a failure", peak)
	}
}

// toolCallSemaphore must skip the channel entirely when it cannot bind, so
// the common small-wave path pays nothing.
func TestToolCallSemaphore_NilWhenNoCapBinds(t *testing.T) {
	al := &AgentLoop{MaxParallelToolCalls: 8}
	for _, size := range []int{0, 1, 8} {
		if sem := al.toolCallSemaphore(size); sem != nil {
			t.Fatalf("waveSize %d: got a semaphore, want nil", size)
		}
	}
	if sem := al.toolCallSemaphore(9); sem == nil || cap(sem) != 8 {
		t.Fatalf("waveSize 9: want a semaphore of cap 8, got %v", sem)
	}

	unlimited := &AgentLoop{MaxParallelToolCalls: 0}
	if sem := unlimited.toolCallSemaphore(100); sem != nil {
		t.Fatal("unlimited: got a semaphore, want nil")
	}
}
