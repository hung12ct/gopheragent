---
name: release-notes
description: Draft release notes from a changelog or commit range. Use when the user asks to write, summarize, or clean up release notes for a version.
---

# Release notes

Group changes under `Added`, `Changed`, `Fixed`, `Removed` — in that order,
omitting any section that would be empty.

Rules that matter more than they look:

- **Lead with the user-visible effect, not the implementation.** "Sessions
  survive a restart" beats "swapped the session store for MySQL".
- **One line per change.** If a change needs two lines, it is two changes.
- **Name breaking changes explicitly**, with the migration in the same line.
- **Drop internal churn.** Refactors, test additions, and CI changes do not
  belong in notes users read.

Date every version. Never ship an "Unreleased" heading.
