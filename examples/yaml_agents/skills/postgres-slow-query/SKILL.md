---
name: postgres-slow-query
description: Diagnose a slow Postgres query. Use when the user says a query, report, or endpoint is slow, is timing out, or asks how to read an EXPLAIN plan or add an index.
---

# Diagnosing a slow Postgres query

Never add an index before step 2. Most "slow query" reports are a plan
problem, and an index added blind makes writes slower without fixing reads.

1. **Get the real plan.** `EXPLAIN (ANALYZE, BUFFERS)` — not plain `EXPLAIN`,
   which shows estimates rather than what actually happened.
2. **Compare rows estimated vs actual.** A ratio worse than 100x means the
   planner is working from bad statistics. Run `ANALYZE` on the table before
   doing anything else.
3. **Read Buffers, not just time.** High `shared read` means it went to disk;
   high `shared hit` with slow wall-clock means the problem is CPU, usually a
   sort or a function in the WHERE clause.
4. **Only then consider an index.** Column order follows the query's equality
   predicates first, then the range predicate, then the sort.

Flag a sequential scan on a large table only if the plan also shows a high
rows-removed-by-filter count — a seq scan returning most of the table is
correct behavior, not a bug.
