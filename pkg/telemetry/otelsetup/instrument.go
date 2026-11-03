package otelsetup

import (
	"github.com/hung12ct/gopheragent/internal/latewire"
	"github.com/hung12ct/gopheragent/pkg/agent"
	"github.com/hung12ct/gopheragent/pkg/telemetry/otelllm"
	"github.com/hung12ct/gopheragent/pkg/telemetry/oteltools"
)

// Instrument wires full tracing and metrics into an already-built loop in one
// call: run and iteration spans on the loop, a chat span plus token metrics
// around loop.LLM, and an execute_tool span plus latency on every tool in
// loop.Tools. Pass otelllm options to label the model, e.g.
//
//	tel.Instrument(loop, otelllm.WithSystem("anthropic"), otelllm.WithModel(model))
//
// Call it once the loop is configured and its tools are registered; tools
// registered later are not instrumented (a warning names them on the loop's
// first event). Calling it twice is harmless. When
// telemetry is disabled (no endpoint configured) it changes nothing, so it is
// safe to call unconditionally. Returns loop for chaining.
//
// Sub-agent tools (builtin.CallSubAgentTool) hold their own LLM: wrap it with
// otelllm.NewProvider when constructing the tool to get the worker's chat
// spans. Worker run spans and metrics are inherited automatically.
func (p *Providers) Instrument(loop *agent.AgentLoop, llmOpts ...otelllm.Option) *agent.AgentLoop {
	if p == nil || !p.Enabled || loop == nil {
		return loop
	}
	latewire.Watch(loop, "otelsetup", false)
	loop.Configure(agent.WithTracer(p.Tracer), agent.WithMeter(p.Meter))
	if loop.LLM != nil {
		opts := append([]otelllm.Option{otelllm.WithTracer(p.Tracer), otelllm.WithMeter(p.Meter)}, llmOpts...)
		loop.LLM = otelllm.NewProvider(loop.LLM, opts...)
	}
	if loop.Tools != nil {
		loop.Tools.Use(oteltools.Instrument(oteltools.WithTracer(p.Tracer), oteltools.WithMeter(p.Meter)))
	}
	return loop
}
