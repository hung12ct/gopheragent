# Agent evaluation example

Two ways to evaluate an agent with `pkg/eval`.

## Mode A — inside `go test` (deterministic, no API keys)

`eval_test.go` builds a suite in Go and runs it against a scripted provider,
so it passes in CI with no credentials:

```bash
go test ./examples/agent_eval/ -v
```

`eval.RunT` turns each task into a subtest — a failing grader points `go test`
straight at what broke.

## Mode B — the CLI against a real provider (adopter CI gate)

`cmd/gopherevals` runs a YAML suite against an agent, writes reports, and exits
non-zero below the pass-rate threshold:

```bash
export LLM_PROVIDER=anthropic ANTHROPIC_MODEL=claude-sonnet-5
go run ./cmd/gopherevals \
  -suite   examples/agent_eval/suite.yaml \
  -agent   examples/agent_eval/agent.yaml \
  -junit   eval-results.xml \
  -md      eval-report.md \
  -threshold 0.9
```

The provider named by `LLM_PROVIDER` doubles as the judge model. Suite files
never name a provider or key — the judge is injected from the environment.

### Two-phase: run once, re-grade many times

Capture transcripts on a live run, then iterate on a rubric offline without
paying for more agent runs:

```bash
go run ./cmd/gopherevals -suite suite.yaml -agent agent.yaml -save-transcripts run.jsonl
# edit the rubric in suite.yaml, then:
go run ./cmd/gopherevals -suite suite.yaml -from-transcripts run.jsonl -md -
```

### GitHub Actions

```yaml
- name: Agent evals
  env:
    LLM_PROVIDER: anthropic
    ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
  run: go run ./cmd/gopherevals -suite suite.yaml -agent agent.yaml -junit eval.xml -threshold 0.9
- name: Publish results
  if: always()
  uses: mikepenz/action-junit-report@v4
  with:
    report_paths: eval.xml
```
