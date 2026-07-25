# Skills — progressive disclosure

Reference material in a system prompt is paid for on every request, whether the
turn needs it or not. An agent with twenty documented procedures spends twenty
procedures' worth of tokens to answer "what time is it".

Skills split that into three tiers:

| Tier | What loads | When | Typical cost |
|---|---|---|---|
| 1. Catalog | name + description | in the system prompt, always | ~50–100 tokens per skill |
| 2. Instructions | the full `SKILL.md` body | on a `read_skill` call | ~2k tokens, once |
| 3. Resources | one bundled file | on a `read_skill_file` call | only what is asked for |

Twenty skills cost twenty descriptions plus the one body actually used.

GopherAgent implements the [Agent Skills](https://agentskills.io) format, so
skills written for other agent products load unchanged.

## Layout

A skill is a directory containing a file named exactly `SKILL.md`:

```
skills/
└── incident-response/
    ├── SKILL.md              # required
    ├── references/
    │   └── runbook.md        # loaded only on request
    └── scripts/
        └── collect.sh
```

```markdown
---
name: incident-response
description: Triage a production incident. Use when the user reports an outage, elevated error rates, or a failing deploy.
---

# Incident response

1. Check the error budget dashboard.
2. Identify the last deploy before the alert fired.
3. See `references/runbook.md` for the rollback procedure.
```

`name` must match the directory name, is 1–64 characters of `[A-Za-z0-9-]`, and
`description` is capped at 1024. The description does all the work: it is the
only skill text the model sees before deciding, so write it as *what this does
and when to reach for it*, not just a title.

Unknown frontmatter keys are rejected by default — an unexpected key is almost
always a typo in a key meant to do something. Optional fields: `license`,
`compatibility`, `metadata`, `allowed-tools`.

## From YAML

```yaml
agent:
  name: "Ops Assistant"
  system_prompt: |
    You are an operations assistant.
  skills:
    sources:
      - ./skills
```

That is the whole integration. The catalog is appended to `system_prompt`, and
`read_skill` (plus `read_skill_file`, if any skill bundles files) is registered
automatically.

Use `BuildFromYAMLContext` when a config declares `skills:` — loading reads the
filesystem, and it is the variant that can carry a deadline:

```go
loop, sm, cfg, err := builder.BuildFromYAMLContext(ctx, "agent.yaml", catalog, provider, nil, nil)
```

## From Go

`pkg/skills` loads from any `io/fs.FS`, not from a directory path. That is what
makes it work outside a CLI:

```go
set, err := skills.FromFS(ctx, os.DirFS("./skills"), skills.TrustedSource())  // disk

//go:embed skills
var skillFS embed.FS
set, err := skills.FromFS(ctx, skillFS, skills.TrustedSource())               // in the binary

set, err := skills.FromFS(ctx, tenantFS)                                      // database, S3, …
```

An `embed.FS` needs no filesystem at all, so skills ship inside the binary and
work in a distroless image. A custom `fs.FS` covers per-tenant content. And
because `fs.ValidPath` rejects `..` and absolute paths at the stdlib boundary,
path traversal is structurally impossible rather than defended against.

Wiring by hand:

```go
set, err := skills.FromFS(ctx, os.DirFS("./skills"), skills.TrustedSource())
if err != nil {
    return err
}
for _, s := range set.Skipped() {
    log.Printf("skill skipped: %s", s)   // see "Debugging" below
}

prompt := builder.WithSkillCatalog(basePrompt, set)
sm := history.NewInMemSessionManager(prompt)

reg := tools.NewRegistry()
builtin.RegisterSkillTools(reg, set)
loop := agent.NewAgentLoop(sm, reg, provider)
```

A `*Set` is immutable and safe to share across every session in a process. Load
it once at construction — never per request.

For skills that come from a database or an API rather than a filesystem,
`skills.New` takes them in memory.

## Trust

Skills are instructions. A skill from a repository you just cloned is untrusted
input, not orders — so trust is **declared by the caller**, never inferred from
a path:

```go
skills.FromFS(ctx, mine, skills.TrustedSource())   // content you control
skills.FromFS(ctx, cloned)                          // untrusted (the default)
```

`Untrusted` is the zero value, so forgetting the option fails closed.

```yaml
sources:
  - ./skills                  # shorthand — trusted, since you typed it here
  - dir: ./vendor-skills
    trust: untrusted
```

Untrusted skills still load. What changes is the path the model takes to reach
them: they appear under a separate `read_skill_untrusted` tool carrying
`RequiresConfirmation`, so your HITL gate fires and a human sees the
instructions first. The two tools have disjoint name enums, and trust is
re-checked on execute, so the model cannot route around the gate.

Two things GopherAgent will not do, both permanent:

- **Honor `allowed-tools`.** It is a privilege grant authored by the content it
  would privilege. `Skill.AllowedTools` is advisory metadata — intersect it with
  your registry to *restrict* a skill, never to *grant*.
- **Execute shell commands embedded in a `SKILL.md`.** Some implementations
  substitute command output into the body at load time. That turns cloning a
  repository into arbitrary code execution.

Sources merge in declaration order and **the first to claim a name wins**, which
is the opposite of the CLI tools' "project overrides user". For a library that
ordering is a one-line hijack — a `deploy` skill in a cloned repo would silently
replace yours. **List trusted sources first.**

## If you use a tools.Selector

A `Selector` replaces the tool registry per call with its top-K matches. The
skill tools rank poorly by construction: the Selector embeds a *tool's* name and
description, but the domain signal lives in the *skill* descriptions. Unpinned,
activation vanishes on exactly the turns that need it, and the model is left
reading a catalog it cannot act on.

```go
sel, _ := tools.NewSelector(ctx, reg, embedder, 8,
    tools.WithPinned(builtin.SkillToolNames()...))
```

The builder logs a reminder at startup because it cannot pin on your behalf —
the Selector is constructed after the loop.

## Bounds

The catalog is in the prompt on every request and bodies stay resident, so both
are capped. Defaults:

| Bound | Default | Why |
|---|---|---|
| `MaxSkills` | 128 | stops a runaway tree at a known count |
| `MaxSkillBytes` | 64 KiB | one `SKILL.md`; the spec suggests bodies under ~5k tokens |
| `MaxCatalogBytes` | 32 KiB | **the one that matters** — see below |
| `MaxResourceBytes` | 256 KiB | per `read_skill_file`; larger content truncates |
| `MaxFilesPerSkill` | 256 | captured resource paths |
| `MaxDepth` | 8 | directory descent |

`MaxCatalogBytes` is the important one. Without it, 128 skills carrying
maximum-length descriptions would render roughly 45,000 prompt tokens on *every*
call, inside the cached prefix. Admission happens at load time, so `Catalog()`,
`Names()`, and the tool enum always agree on which skills exist.

Resident bodies are bounded by `MaxSkills × MaxSkillBytes` — 8 MiB worst case,
around 1 MiB in practice.

## Debugging

Loading tolerates a malformed skill rather than failing the process: one bad
`SKILL.md` must not stop an agent from starting. That trade only works if
rejections stay visible.

```go
for _, s := range set.Skipped() {
    log.Printf("skill skipped: %s", s)
}
```

Reasons include invalid frontmatter, a name that does not match its directory,
a duplicate name from a later source, an oversized file, and budget exhaustion.
The YAML builder logs these for you at startup.

One deliberate leniency: `description: Use when: the user asks about X` is not
valid YAML, but it is the format's own idiom and appears throughout published
skills. When strict parsing fails, a fallback scanner recovers `name` and
`description` — and nothing else. A file whose YAML does not parse is precisely
where a privilege grant must not be read out of half-understood text.
`Skill.Lenient` reports when that happened.

## Measuring whether it actually works

Everything above is mechanical: the catalog renders, the tool returns a body.
None of it proves the thing the feature rests on — that a real model reaches
for the right skill from its description alone. That is a property of your
descriptions, not of this package, so it has to be measured rather than
assumed.

`pkg/eval` already does this. Activation is a trajectory assertion:

```go
// should activate, and specifically this one
eval.Trajectory(eval.MatchSubset, []eval.ExpectedCall{
    {Name: "read_skill", Args: eval.ArgsSubset(map[string]any{
        "name": "incident-response",
    })},
})

// should activate nothing — MatchSuperset with an empty expected set means
// every actual call must match something expected, and nothing is
eval.Trajectory(eval.MatchSuperset, nil)
```

**Test both directions.** A model that never activates makes the feature
useless, but a model that activates for *everything* is worse: it pays a
body's tokens on every turn and inverts the saving. Include trivia and
greetings that should load nothing, and near-misses — a question that
mentions deploys without being an incident.

**Read pass^k, not pass@k.** With `Trials: 3`, pass@k means one lucky trial
out of three. pass^k means every trial activated. The gap between them is the
signal:

| Result | What it means |
|---|---|
| pass^k | the description reliably carries the decision |
| pass@k but not pass^k | the description is ambiguous — rewrite it |
| neither | the description is not doing its job at all |

`TaskReport.PassAllK` is computed for every task, so you can name the
inconsistent ones and iterate on their wording.

A worked suite lives in `examples/agent_eval/skills_eval_test.go`. It skips
without `OPENAI_API_KEY` so CI stays green:

```bash
OPENAI_API_KEY=... go test ./examples/agent_eval/ -run TestSkillsActivation -v
```

Measured against `gpt-4o-mini` with the four example skills: **8/8 tasks at
pass^k over 3 trials each** — every positive case activated the right skill on
every trial, and every negative case activated nothing.

One caveat when you build your own suite: confirm the graders can fail before
trusting a green result. Assert the *wrong* skill, or assert no-activation on
a case that does activate, and check those fail. A grader that passes
vacuously proves nothing.

### What it costs

For those four skills:

```
catalog (tier 1):  1072 chars  ~268 tokens   paid every turn
all bodies:        3299 chars  ~824 tokens   the old all-or-nothing cost
largest body:      1020 chars  ~255 tokens   what one activation costs
```

A 68% per-turn reduction at four skills, and the gap widens with each skill
added — the catalog grows by one description while the alternative grows by a
whole body.
