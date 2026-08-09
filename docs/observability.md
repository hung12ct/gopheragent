# Observability — OpenTelemetry traces & metrics

GopherAgent emits OpenTelemetry **traces** and **metrics** for the whole agent
run. One conversation turn is a single trace:

```
agent.run                    ← per turn, tagged gopheragent.session.key
└── agent.iteration          ← per ReAct step
    ├── chat <model>         ← LLM call: latency + input/output token attrs
    └── execute_tool <name>  ← each tool: latency
```

The core imports only the OpenTelemetry **API**, so instrumentation is a no-op
(and allocation-free) until you wire a provider. The SDK and OTLP exporters live
only in `pkg/telemetry/otelsetup`, so a library consumer that never enables
telemetry does not link them.

## Enable it

`otelsetup.Setup` builds an OTLP TracerProvider + MeterProvider and returns
ready-to-use handles plus a shutdown func. Endpoint and credentials follow the
standard `OTEL_*` environment variables.

```go
tel, shutdown, err := otelsetup.Setup(ctx, otelsetup.Config{ServiceName: "my-agent"})
if err != nil { /* ... */ }
defer shutdown(context.Background())
```

### Loops built with `agent.New`

```go
llm := otelllm.NewProvider(p,
    otelllm.WithSystem("anthropic"), otelllm.WithModel(model),
    otelllm.WithTracer(tel.Tracer), otelllm.WithMeter(tel.Meter))

reg.Register(tools.Chain(myTool,
    oteltools.Instrument(oteltools.WithTracer(tel.Tracer), oteltools.WithMeter(tel.Meter))))

loop := agent.New(sm, reg, llm,
    agent.WithTracer(tel.Tracer), agent.WithMeter(tel.Meter))
```

### Loops built with the YAML builder

The builder takes no options, so wire the same three pieces via `Configure`
(on the loop) and `Use` (on the catalog):

```go
catalog.Use(oteltools.Instrument(
    oteltools.WithTracer(tel.Tracer), oteltools.WithMeter(tel.Meter)))

loop, _, _, _ := builder.BuildFromYAMLWithSession(path, catalog,
    otelllm.NewProvider(provider, otelllm.WithTracer(tel.Tracer), otelllm.WithMeter(tel.Meter)),
    hook, sm)

loop.Configure(agent.WithTracer(tel.Tracer), agent.WithMeter(tel.Meter))
```

## Run a backend locally

Grafana's OTel-LGTM image bundles an OTLP collector + Tempo (traces) +
Prometheus (metrics) + a Grafana UI:

```bash
docker run -d --name lgtm -p 3000:3000 -p 4317:4317 -p 4318:4318 grafana/otel-lgtm
export OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317
```

Grafana → http://localhost:3000 (admin/admin). The `examples/demo` app enables
export automatically when `OTEL_EXPORTER_OTLP_ENDPOINT` is set.

## Debugging a reported conversation

Every span carries the session identifier as `gopheragent.session.key`, and each
turn is one trace. To find what happened in a reported conversation, filter on
that attribute:

```
Tempo (TraceQL):   { .gopheragent.session.key = "<session-key>" }
Jaeger:            Tags → gopheragent.session.key=<session-key>
```

Open the trace to see the full `run → iteration → chat → tool` tree with
per-step latency. For exact message content (prompts, completions, tool
arguments/results) read the session history — spans intentionally carry
metadata, not payloads, to keep PII and cost out of the telemetry backend.

## Metrics

Recorded on the meter; via the OTLP→Prometheus bridge the names gain the
conventional `_seconds` / `_total` suffixes:

| Instrument | Prometheus name | Labels |
|---|---|---|
| LLM call latency | `gen_ai_client_operation_duration_seconds` | `gen_ai.system`, `gen_ai.request.model` |
| LLM token usage | `gen_ai_client_token_usage_total` | `gen_ai.token.type` = input\|output |
| Tool latency | `gopheragent_tool_execution_duration_seconds` | `gen_ai.tool.name` |
| Tool errors | `gopheragent_tool_errors_total` | `gen_ai.tool.name` |
| Iteration latency | `gopheragent_agent_iteration_duration_seconds` | — |

Duration histograms ship LLM-tuned explicit buckets (`0.01 … 81.92` s, the
GenAI-convention ladder) instead of the SDK's millisecond defaults, so
percentiles stay meaningful for second-scale LLM latencies.

### Starter PromQL

```promql
# LLM p95 latency
histogram_quantile(0.95, sum(rate(gen_ai_client_operation_duration_seconds_bucket[5m])) by (le))

# Token burn rate, input vs output
sum(rate(gen_ai_client_token_usage_total[5m])) by (gen_ai_token_type)

# Tool error rate
sum(rate(gopheragent_tool_errors_total[5m])) by (gen_ai_tool_name)
```

## Token-budget endpoint (no collector)

Independently of OTLP, a `BudgetTracker` exposes per-session token usage as a
Prometheus/OpenMetrics text endpoint:

```go
bt := agent.NewBudgetTracker(100_000)
loop.OnEvent(bt.Handler())
http.Handle("/metrics", agentmetrics.Handler(bt))
```

## Per-Run cost

`RunCostEvent` fires once per Run — on every terminal path, not just the
success one — with rolled-up tokens and a dollar total. There are two ways it
gets a figure, and they compose:

```go
// Estimate from your own rates, for providers that bill silently.
loop := agent.New(sm, reg, provider,
    agent.WithPriceTable(agent.PriceTable{
        "claude-sonnet-4-6": {InputPerMTokens: 3, OutputPerMTokens: 15},
    }, "claude-sonnet-4-6"))
```

A provider that knows what it charged reports it directly instead, by setting
`CostUSD` on the `TokenUsage` it returns from `GenerateStream`. Gateways that
route across vendors typically do. No `PriceTable` is needed in that case — a
static table could not price them correctly anyway, since it cannot see which
model the gateway picked or which cache discounts applied.

Dollars resolve **per call**, then sum: a call that reported `CostUSD`
contributes that exact charge, and every other call is estimated from the
table. A Run that mixes both bills each call the best way available. Read
`RunCostEvent.Usage.CostUSD` to see how much of `USD` was billed rather than
estimated — equal means the total is exact, zero means it is all estimate.

With neither a table nor a reporting provider, no event fires; raw counts are
still on the wire as `UsageEvent`.

## Notes

- **Zero-cost when off:** no tracer/meter → decorators return the wrapped
  object unchanged, middleware is identity, and the loop opens no spans.
- **Sampling & cardinality:** at high volume set an OTel sampler
  (`OTEL_TRACES_SAMPLER=parentbased_traceidratio`,
  `OTEL_TRACES_SAMPLER_ARG=0.1`). The OTLP metrics do not carry `session_key`;
  the `agentmetrics` token endpoint does, so evict per-session keys if they are
  short-lived.
- **Lightweight alternative:** `telemetry.NewOTelHandler(tracer)` attaches via
  `loop.OnEvent` and produces coarse per-iteration spans without the decorator/
  middleware. Prefer the `WithTracer` path above for nested LLM/tool spans; the
  two are mutually exclusive.
- **Verify your wiring:** `pkg/telemetry/otelsetup/live_verify_test.go` is an
  `OTEL_LIVE`-gated end-to-end export test against a real collector.
