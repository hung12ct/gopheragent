# Changelog

All notable changes to GopherAgent are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/); versions follow [Semantic Versioning](https://semver.org/) — pre-1.0, breaking API changes only require a minor bump.

## [Unreleased]

## [v0.14.2] — 2026-05-03

### Fixed
- `AsyncTaskManager.StartTask` now uses `context.WithoutCancel(parentCtx)` instead of a fresh `context.Background()`. Caller-installed values (user_id, request_id, tracer spans, dynamic-context func, sub-agent emitter) flow into the worker. Async-detachment invariant (parent cancel does not kill worker) is preserved. (#69)

## [v0.14.1] — 2026-05-03

### Fixed
- `AgentLoop.runIteration` no longer persists the pruned message slice. `enforceTokenBudget`'s output now feeds the immediate LLM call only; `*msgs` (and therefore `SetHistory` / `saveSession`) keep the original full-fidelity history. Previously every soft-trim shrank stored tool messages permanently, degrading long sessions irreversibly. (#68)

## [v0.14.0] — 2026-05-02

Multi-user, long-running, audit-friendly chat surface — the foundation for sidebar UX, soft-delete + undo, prompt-rule auditability, and unbounded session length.

### Added
- `EventTypeSessionCreated` / `SessionCreatedEvent` — fires as the first frame of a stream when GetHistory returns an empty or system-only slice. Replaces the brittle "history was empty pre-call" race-prone detection. Visitor gains `VisitSessionCreated`. (#65)
- `builtin.GenerateTitle` — one-shot helper over `LLMProvider.GenerateStream` that drains content events, normalizes the result (strips quotes, trims trailing punctuation, bounds by rune count), returns `ErrEmptyTitle` on empty. Pairs with `session_created`. (#65)
- `SessionManager.Query(ctx, prefix, opts) ([]SessionMeta, error)` — prefix-scoped session listing for sidebar UX. Supports limit/offset/sort. Backed by `WHERE session_key LIKE ?` with proper LIKE-escaping. Includes `idx_updated_at` MySQL index for the recent-first sort path. (#66)
- `SessionManager.SoftDelete` / `Restore` / `PurgeDeletedBefore` — reversible delete semantics. Reads filter out deleted sessions by default; pass `IncludeDeleted: true` to surface them. PurgeDeletedBefore bounds storage growth. (#66)
- `PromptVersion` field + `WithPromptVersion` builders on all backends. Stamps `<!-- prompt-version:V -->` HTML-comment marker in stored system prompts so operators can grep storage to audit which sessions are running which version. Bump the version when editing prompt rules. (#64)
- `MessageStore` interface + `MySQLAppendOnlyMessageStore` + `WithMySQLMessageStore` option. Decouples per-message persistence from the session row. `persistedCount` hint makes each Save touch only the new tail rows. Default keeps whole-blob behavior for small products; opt in for products that need long-session scaling without the JSON-column row-size cap. (#67)

### Changed (breaking)
- `SessionManager` interface gains `Query`, `SoftDelete`, `Restore`, `PurgeDeletedBefore`. Custom implementations must add these.
- `EventVisitor` interface gains `VisitSessionCreated`.

## [v0.13.0] — 2026-05-02

### Added
- `EventTypeMaxItersReached` / `MaxItersReachedEvent` — typed signal alongside the legacy `ErrMaxIterations` error event so SSE consumers can distinguish hitting the iteration cap from generic errors without sniffing error strings. Visitor gains `VisitMaxItersReached`. (#62)
- `AgentLoop.MaxToolCallsPerSession int` (0 = unlimited) — caps cumulative tool calls across all iterations of a single Run; emits `ErrMaxToolCallsPerSession` and saves session on cross. Distinct from `MaxToolCallsPerTurn` (per-iteration wave). (#62)
- `AgentLoop.OnToolResult ToolResultHook` — fires after every successful tool execution; can rewrite the result string or veto via non-nil error. Eliminates wrap-just-to-substitute-output patterns (URL rewrites, redaction, format normalization). (#63)
- `WithDynamicContextFunc(ctx, fn)` + `DynamicContextFuncFromContext(ctx)` — parent loop stamps `DynamicContext` on each tool ctx; `SQLAgentTool` and `CallSubAgentTool` pull it off and install it on their worker loop, so time-sensitive context like "today is …" flows down through agent hierarchies. (#61)
- `StreamEvent.Name` (JSON `name`) and `ToolCallEvent.Name` carry the bare tool identifier on `tool_call` events. Consumers no longer parse `"Executing: <name>"` from `Content`. (#60)

### Changed (breaking)
- `EventVisitor` interface gains `VisitMaxItersReached`.

## [v0.12.0] and earlier

- `SQLAgentTool` builder methods (`WithName` / `WithDisplay` / `WithRequiresConfirmation`) — multi-instance + autonomous-agent use without wrapper structs.
- Directive permission-denied error message — model now reports the permission gap to the user instead of silently falling back to workarounds.
- README section on the permission flow — documents `RequiresConfirmation` × `ConfirmHITL` × `Permissions` interaction.
- Enum struct tag support in `tools.SchemaFor[T]()` — emit values into JSON-Schema's `enum` array so providers reject invalid values upstream.

[Unreleased]: https://github.com/hung12ct/gopheragent/compare/v0.14.2...HEAD
[v0.14.2]: https://github.com/hung12ct/gopheragent/releases/tag/v0.14.2
[v0.14.1]: https://github.com/hung12ct/gopheragent/releases/tag/v0.14.1
[v0.14.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.14.0
[v0.13.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.13.0
