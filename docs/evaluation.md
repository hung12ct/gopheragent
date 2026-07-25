# Evaluation

`pkg/eval` evaluates an agent against a suite of tasks. It checks:

- the tool-call **trajectory** — which tools ran, in what order, with what
  arguments (five match modes: strict, in-order, unordered, subset, superset);
- the final **answer** — `Contains` / `Regexp` / `Exact`, or an LLM-as-judge
  with an `unknown` escape hatch and N-sample majority vote;
- whether a **human-in-the-loop approval gate** fired for a dangerous tool (or,
  with `NoHITL`, that a safe request did *not* trip one);
- and reports **cost, tokens, and latency**.

Tasks can be multi-turn conversations sharing one session, run over N trials
with pass@k / pass^k reported, and executed with bounded concurrency.

## Run it two ways

```go
// Inside go test — deterministic with a scripted provider, no API keys.
eval.RunT(t, &eval.Runner{NewTarget: factory}, suite)
```

```bash
# In CI — YAML suite against a real agent; exits non-zero below threshold.
go run ./cmd/gopherevals -suite suite.yaml -agent agent.yaml -junit eval.xml -threshold 0.9
```

Reports emit as JSON, JUnit XML (rendered natively by GitHub Actions), and
Markdown. Capture transcripts once with `-save-transcripts` and re-grade them
against a revised rubric via `-from-transcripts` without re-running the agent.
See [`examples/agent_eval`](../examples/agent_eval).
