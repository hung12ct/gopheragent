---
name: api-versioning
description: Plan a breaking API change. Use when the user asks how to version an endpoint, deprecate a field, or roll out a change that would break existing clients.
---

# Shipping a breaking API change

The goal is that no client breaks without having had a window to migrate.

1. **Decide whether it is actually breaking.** Adding an optional field is
   not. Removing a field, renaming one, tightening validation, or changing a
   default all are.
2. **Add the new shape alongside the old.** Both serve simultaneously; the
   old one keeps working unchanged.
3. **Instrument the old path** before announcing anything. You need to know
   who is still calling it, or the deprecation window is a guess.
4. **Announce with a date**, not a release number. Clients schedule against
   calendars.
5. **Remove only when the old path's traffic is zero** for a full billing
   period, not merely low.

Never version by adding `/v2` to the whole API when one endpoint changed —
that forces every client to migrate everything.
