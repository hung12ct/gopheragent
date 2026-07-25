---
name: incident-response
description: Triage a production incident. Use when the user reports an outage, elevated error rates, a failing deploy, or asks how to roll back.
---

# Incident response

Work the steps in order. Do not skip to a fix before step 2 — most incidents
are the last deploy, and rolling that back is faster than diagnosing forward.

1. **Establish blast radius.** Which endpoints, which regions, what percentage
   of requests. Say the numbers back to the user before proposing anything.
2. **Find the last deploy before the alert fired.** Compare the alert's first
   firing timestamp against the deploy log.
3. **Decide roll back or roll forward.** Roll back by default. Roll forward
   only when the deploy contains a migration that cannot be reversed.
4. **Communicate.** Post the blast radius, the suspected cause, and the ETA
   before starting the remediation, not after.

For the rollback commands themselves, read `references/runbook.md`.
