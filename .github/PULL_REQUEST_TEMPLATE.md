<!--
Title must follow Conventional Commits, e.g. `feat(agent): add best-of-K runner`
or `fix(llm): classify auth errors as ErrLLMAuth`.
-->

## What & why

<!-- One or two sentences: what this changes and the problem it solves. Link the issue. -->

Closes #

## Changes

<!-- Bullet the notable changes. One logical change per PR — split unrelated work. -->

-

## Testing

<!-- How you verified this. Name the tests you added/ran. Goroutine/mutex code must pass under -race. -->

-

## Checklist

- [ ] Title is a Conventional Commit (`feat:`, `fix:`, `docs:`, `refactor:`, `chore:`, …)
- [ ] `gofmt -l .` prints nothing
- [ ] `make lint` is clean
- [ ] `make build` passes
- [ ] `make test` (or `make test-race` for concurrency changes) passes
- [ ] Errors wrapped with a package prefix (`fmt.Errorf("<pkg>: ...: %w", err)`)
- [ ] `CHANGELOG.md` updated **if** this PR is cut as a release tag
- [ ] No changes to `pkg/llm/`, `pkg/history/`, or `pkg/telemetry/` without explicit maintainer approval
