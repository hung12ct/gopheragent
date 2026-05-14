# Changelog

All notable changes to GopherAgent are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/); versions follow [Semantic Versioning](https://semver.org/) — pre-1.0, breaking API changes only require a minor bump.

## [v0.23.0] — 2026-05-15

First-class `Regenerate` / `Continue` affordances on `AgentLoop` so chat-UI adopters get the "redo my last turn" and "keep going from where it stopped" patterns without re-implementing tool_use/tool_result-safe history truncation in adopter code (the exact shape that produced the Anthropic 400s sanitized in v0.20.1).

### Added
- `AgentLoop.Regenerate(ctx, sessionKey, streamChan) error` — rewinds the session to just before the last user message using safe-boundary truncation, persists the rewound history before any new tool runs, and replays the recovered user message through a fresh iteration. Returns `ErrNothingToRegenerate` without touching history or the stream when no user message exists. Stream lifecycle matches `RunIterationStream` (library owns the channel and closes it on completion).
- `AgentLoop.Continue(ctx, sessionKey, streamChan) error` — resumes the loop on the current persisted history without appending a new user message. Intended for "the previous run hit `MaxIters` / `MaxToolCallsPerSession` / a HITL timeout / a user-issued Stop and the user wants to keep going." Returns `ErrNothingToContinue` when the session's last message is a clean final-assistant turn. `PatchDanglingToolCalls` is applied to the live history before iteration so any half-finished tool wave from the prior run is sealed with synthetic results before the next LLM call.
- `EventTypeRegenerated` / `RegeneratedEvent{PreviousAssistantIndex, TruncatedAt}` and `EventTypeContinued` / `ContinuedEvent{ContinuedFromIndex}` — typed transition events emitted as the first frame of their respective streams so UIs can mark the superseded bubble or anchor "resumed from here" rendering before replacement content arrives.
- `ErrNothingToRegenerate` / `ErrNothingToContinue` sentinels — match with `errors.Is` to distinguish "the call couldn't run" from a normal terminal error event.
- `history.SafeTruncate(msgs, atIndex) int` — public alias over the internal `snapToSafeBoundary` that `SessionForker.Fork` already uses. `Regenerate` and `Continue` go through the same helper so the in-place rewind and the fork path share one source of truth for tool-pair safety.

### Changed (breaking)
- `EventVisitor` interface gains `VisitRegenerated(RegeneratedEvent)` and `VisitContinued(ContinuedEvent)`. Adopters implementing `EventVisitor` directly must add both methods; consumers using the type-switch on `StreamEvent.Payload()` are unaffected (the new payloads round-trip through `EventPayload` like every other typed event).

## [v0.22.0] — 2026-05-14

Streaming termination + HITL clarity batch — the agent loop's cancellation path now always delivers a terminal frame, and the HITL gate distinguishes timeout from denial with typed events that bubble cleanly through sub-agent wrappers so the outer chat agent sees the real cause instead of the inner worker's paraphrase.

### Added
- `AgentLoop.ConfirmHITLTimeout time.Duration` (default 0 = no timeout) — caps how long the HITL gate waits for `ConfirmHITL` to return. When set, the gate wraps the callback ctx with this deadline; a callback that honors ctx returns false on expiry, the gate emits the typed timeout event, and the model receives a timeout-specific directive distinct from a user denial ("the approval prompt expired — ask the user to retry when ready"). Zero-cost when unused. (#98)
- `agent.WithConfirmHITLTimeout(ctx, d)` + `agent.ConfirmHITLTimeoutFromContext(ctx)` — mirrors the existing `WithConfirmHITL` pattern so sub-agent worker loops inherit the parent's approval window without manual plumbing. `CallSQLAgentTool` and `CallSubAgentTool` now wire it on construction alongside `ConfirmHITL`. (#98)
- `EventTypeHITLDenied` / `HITLDeniedEvent` and `EventTypeHITLTimedOut` / `HITLTimedOutEvent` — typed observational signals emitted from the HITL gate so adopters can route on cause (denial vs. timeout) without scraping tool-message text. `HITLTimedOutEvent.Timeout` carries the configured window so UI can render "approval expired after 2m" without reaching into agent state. (#98)
- `agent.HITLTimedOutError` + `agent.ErrHITLTimedOut` sentinel — typed error parallel to `HITLDeniedError` / `ErrHITLDenied` so adopters can `errors.Is` on the cause. (#98)
- `CallSQLAgentTool` and `CallSubAgentTool` now prepend an unambiguous `HITL_BLOCKED: timeout — ...` / `HITL_BLOCKED: denied — ...` directive to their return string when the inner HITL gate fires. The outer chat agent receives the structured cause instead of the worker sub-agent's vague paraphrase ("I was unable to execute the query") — closes a UX gap where outer agents would tell users to "click Approve" after the prompt had actually timed out or been denied. Same shape applies to any `CallSubAgentTool` adopter, not just SQL. (#98)

### Changed (breaking)
- `EventVisitor` interface gains `VisitHITLDenied(HITLDeniedEvent)` and `VisitHITLTimedOut(HITLTimedOutEvent)`. Adopters implementing `EventVisitor` directly must add both methods; consumers using the type-switch on `StreamEvent.Payload()` are unaffected. (#98)

### Fixed
- `AgentLoop.RunIterationStream` / `RunIterationStreamMessage` now best-effort forward terminal frames (`done` / `error`) on ctx cancel instead of silently draining the internal channel. Previously, when the consumer's ctx fired, the relayer dropped every pending event — including the loop's `ErrContextCancelled` error event — and just closed `streamChan`. Adopters that distinguish cancellation by terminal frame type now receive the typed error; consumers that infer termination only from channel-close still work. (#97)
- `examples/demo/main.go` SSE handler now synthesizes a terminal frame in a defer when the upstream channel closes without one, and treats top-level `EventTypeError` (Source=="") as terminal alongside `done`. The frontend exits its streaming state regardless of termination path (client disconnect, ctx cancel, natural completion). (#97)

## [v0.21.0] — 2026-05-13

SQL workbench batch — opt-in mutations alongside the read-only verbs, and HITL on the inner SQL statement (not just the natural-language request). The two combine into the "show me the DELETE before it runs" UX adopters building AI data workbenches were rebuilding by hand.

### Added
- `builtin.CallSQLAgentTool.WithAllowMutations(allow bool)` — widens the SQL classifier to accept `INSERT` / `UPDATE` / `DELETE` / `MERGE` alongside the read-only verbs. DDL (`DROP` / `CREATE` / `ALTER` / `TRUNCATE` / `GRANT` / `REVOKE`) stays rejected. Mutation path runs through `ExecContext` and reports affected rows in `SQLResult.RowCount`; `EnsureLimit` is intentionally skipped on mutations (UPDATE/DELETE `LIMIT` semantics are dialect-specific and would silently change meaning). Default false — no regression for existing read-only adopters. Godoc on the builder recommends pairing with `RequiresConfirmation` + `ConfirmHITL` so mutations always reach a human. (#94)
- `builtin.ClassifySQL(sql string) (SQLKind, error)` + `builtin.SQLKind` enum (`SQLKindRead` / `SQLKindMutation` / `SQLKindUnknown`) — adopters needing their own dispatch around the read/mutation split now have a public classifier. `builtin.ValidateReadOnly` keeps its old contract and is a thin wrapper around `ClassifySQL`. (#94)
- `builtin.CallSQLAgentTool.WithExecuteSQLConfirmation(allow bool)` — opt-in HITL on the inner `execute_sql` leaf, so adopters can approve the actual SQL string the model generated rather than (only) the natural-language request at the outer `call_sql_agent` boundary. Workbench UX combines `WithExecuteSQLConfirmation(true).WithRequiresConfirmation(false)` to gate on the statement and skip the intent prompt; the original chatbot UX stays the default (intent approval, single gate). Godoc documents all four flag combinations. (#95)
- `agent.WithConfirmHITL(ctx, fn)` + `agent.ConfirmHITLFromContext(ctx)` — sub-agents previously constructed worker `AgentLoop`s without inheriting the parent's `ConfirmHITL`, so any `RequiresConfirmation=true` tool deep in a sub-agent denied silently with the `ConfirmationGateUnconfigured` directive. The agent executor now stamps `WithConfirmHITL` on every tool's ctx (mirroring the existing `WithDynamicContextFunc` propagation), and `CallSQLAgentTool.runOnce` reads it back to wire the sub-agent's gate. Closes a latent gap that would have been hit by anyone using `CallSubAgentTool` for confirmation-required tools too. (#95)

## [v0.20.2] — 2026-05-13

### Fixed
- `builtin.GenerateTitle` now appends a user-role nudge when the sanitized history ends with an assistant turn. Anthropic rejects calls whose final message is assistant (`"This model does not support assistant message prefill. The conversation must end with a user message."`); after v0.20.1's tool-block strip, the typical `[user, finalAssistant]` shape tripped this rule. The appended nudge (`"Now produce the title for the conversation above. Output only the title — no quotes, no trailing punctuation, no explanation."`) makes titling robust against Anthropic's "must end with user" constraint without changing behavior on OpenAI / Gemini. Idempotent — slices already ending in user (mid-conversation retitle) are forwarded unchanged. Should have shipped together with v0.20.1; recording as a separate release because v0.20.1 is already tagged. (#92)

## [v0.20.1] — 2026-05-13

### Fixed
- `builtin.GenerateTitle` no longer 400s when adopters slice tool-using histories. Anthropic rejects message slices where an assistant `tool_use` is not immediately followed by its paired `tool_result`; the typical auto-title input shape (`[firstUser, firstAssistant-with-toolcall, tool, finalAssistant]`) frequently breaks that adjacency once the conversation is sliced for titling. `GenerateTitle` now strips `role:"tool"` messages and clears `ToolCalls` on assistant messages before forwarding, dropping any assistant message that becomes empty after the strip. Titles only need intent + outcome, so the sanitization is the correct semantic regardless of provider quirks. (#90)

## [v0.20.0] — 2026-05-13

Adopter integration batch — terminal-event/persistence ordering hardened across every exit path, `OnSQL` finally surfaces result rows, stream-channel ownership documented.

### Added
- `SQLQueryEvent.Columns []string`, `SQLQueryEvent.Rows []map[string]any`, `SQLQueryEvent.Truncated bool` — `builtin.CallSQLAgentTool.OnSQL` now fans out the executed result (not just the query string), so adopters mirroring sub-agent results into their own UI (data grid, export panel) no longer need to re-run the query against their own DB handle. Populated on success and on partial-row errors; zero-valued on early validation failure. Existing `Query` / `Error` consumers keep working unchanged.
- `RunIterationStream` / `RunIterationStreamMessage` godoc now spells out channel-lifecycle ownership — the library spawns goroutines and closes `streamChan` exactly once on agent loop termination. Callers MUST NOT close the channel themselves; doing so previously caused `panic: send on closed channel` / `panic: close of closed channel` on the first emitted event.

### Fixed
- Terminal events (`Done`, `Error`, `LimitExhausted`, `MaxItersReached`) now fire **after** `saveSession` at all six exit sites (`handleFinalAnswer`, `runIteration` ctx-cancel / LLM-fail / fatal-loop, `runLogicLoop` MaxToolCallsPerSession / MaxIters). Adopters consuming a terminal event and immediately reading `Sessions.GetHistory(...)` (auto-titling, sidebar refresh, audit pipelines) now observe the durable state instead of racing the persistence write.
- `executeSQLTool` `Execute` no longer split its success/failure exits between two helpers (`notify` + `marshal`) that could drift — refactored into a single `emit` so every return path emits a consistent `SQLQueryEvent` and marshals the same `SQLResult` envelope.

## [v0.19.0] — 2026-05-05

Tool-call observability batch — every dispatch is now correlatable across `tool_call`, `tool_progress`, and `OnToolResult` with a single ID; the hook fires on success and failure; `WithLogging` gains the knobs adopters were writing custom slog handlers for.

### Added
- `StreamEvent.ToolCallID`, `StreamEvent.ArgsJSON`, `StreamEvent.Reused` — agent-generated correlation ID, raw arguments, and a flag indicating the wave executor consumed a speculative result rather than re-dispatching. Mirrored on `ToolCallEvent` as `ID`, `ArgsJSON`, `Reused`. Adopters can now pair entry events to post-execution audit lines reliably even when `SpeculativeTools=true` interleaves parallel calls. (#86)
- `ToolProgressEvent.Name` and `ToolProgressEvent.ToolCallID` — mid-execution progress events now echo the originating dispatch so adopters can attribute `ReportProgress` messages to a specific tool under speculation. (#86)
- `tools.WithToolCallID(ctx, id)` / `tools.ToolCallIDFromContext(ctx)` — typed ctx accessors for the per-Execute correlation ID. Middleware reads it without re-plumbing; the agent loop sets it before invoking the tool. (#86)
- `tools.WithContextExtractor(fn func(context.Context) []slog.Attr)` — `WithLogging` option that surfaces ctx-scoped attrs (trace_id, user_id, tenant tags) on every tool log line in one line of wiring instead of writing a custom `slog.Handler` bridge. (#86)
- `tools.WithSuccessLevel(slog.Level)` — `WithLogging` option that demotes entry ("tool call") and successful exit ("tool ok") records below the `LevelError` baseline. Errors always log at `LevelError`; production handlers at `LevelInfo` keep failure visibility while filtering out healthy traffic. (#86)
- `tools.WithArgsTruncation(maxBytes int)` — `WithLogging` option that caps oversized arguments in log output, replacing the tail with a length marker. Useful for tools accepting image data URIs, multi-KB SQL, etc. (#86)
- `WithLogging` automatically includes `tool_call_id` (read from ctx) and `duration_ms` (exit-line latency) on every record. (#86)

### Changed (breaking)
- `ToolResultHook` signature is now `func(ctx context.Context, toolCallID, toolName, argsJSON, result string, structured any, execErr error) (string, error)`. The hook fires on **every** dispatch — success and failure — eliminating the need for a separate `EventTypeError` listener for adopters wanting "every tool call in one place." Hook semantics: on success, `(rewritten, nil)` rewrites the result and `(_, hookErr)` converts to a tool error; on failure, `(rewritten, nil)` recovers the call (rewritten becomes the LLM-facing result) and `(_, hookErr)` replaces the original error. Existing callers add `_ string` (callID) and `_ error` (execErr) placeholders; success-only hooks guard with `if execErr != nil { return result, nil }`. (#86)
- `tools.WithLogging` now takes a variadic `...LoggingOption`. Existing zero-option callers compile unchanged; configurable callers use the new `With*` helpers above. (#86)

## [v0.18.1] — 2026-05-04

### Fixed
- Speculator now honors `tools.StructuredResult`. With `SpeculativeTools=true`, `spawnSpeculative` previously called `Execute` unconditionally, dropping the typed payload — so `OnToolResult` fired with `structured=nil` for every speculated structured tool. The speculator now mirrors the wave executor's dispatch and threads the payload through `awaitSpeculative`. (#84)

## [v0.18.0] — 2026-05-04

API hygiene follow-up — propagate the constructor-with-required-storage pattern to the consumer types.

### Changed (breaking)
- `builtin.GenerateImageTool.Storage` and `builtin.GenerateVideoTool.Storage` fields **removed** — fields are unexported. Both constructors now take `storage AssetStorage` as a required positional arg with a nil-check at startup, so misconfiguration surfaces before the first generation instead of on the first inline render. Replace `tool, _ := builtin.NewGenerateImageTool(key, model); tool.Storage = s` with `tool, _ := builtin.NewGenerateImageTool(key, model, s)`. Same shape for `NewGenerateVideoTool`. (#82)

## [v0.17.0] — 2026-05-04

API hygiene batch — small interfaces, aligned names, constructor-only built-ins, and a confirmation gate that no longer confabulates user denial.

### Added
- `agent.SessionForker`, `agent.SessionQueryable`, `agent.SoftDeletable` capability interfaces — opt-in surfaces for backends that fork, list, or soft-delete. Custom `SessionManager` implementations now stub only what they actually support; `agent.ForkAtLastUser` type-asserts and surfaces a clear error when the backend doesn't implement `SessionForker`. (#80)
- `agent.ConfirmationGateUnconfiguredError` + `agent.ErrConfirmationGateUnconfigured` sentinel — a tool that requires confirmation while `ConfirmHITL` is nil and no `PermissionAllow` rule resolves the call now writes a misconfig-specific directive to the model (so it doesn't tell the user "you denied this") and logs a one-time warning per `AgentLoop`. (#80)
- `builtin.NewLocalDiskStorage(saveDir, urlBase)` constructor — validates both args at startup and normalizes the `urlBase` trailing slash once at construction instead of on every `Save`. (#80)
- `builtin.NewStartAsyncTaskTool` / `NewCheckAsyncTaskTool` / `NewCancelAsyncTaskTool` / `NewUpdateAsyncTaskTool` / `NewListAsyncTasksTool` — every async tool now constructs through a manager-checked function, matching the rest of the builtin family. (#80)

### Changed (breaking)
- `agent.SessionManager` core interface drops to 6 methods. `Fork`, `Query`, `SoftDelete`, `Restore`, `PurgeDeletedBefore` move to the new capability interfaces. Existing concrete backends (InMem, File, MySQL) and the `historyfake` continue to satisfy every method, so wiring with the shipped backends is unchanged.
- `builtin.SQLAgentTool` renamed to `builtin.CallSQLAgentTool` (constructor `NewCallSQLAgentTool`). The type name now matches the registered tool name `"call_sql_agent"` and the existing `CallSubAgentTool` convention.
- `builtin.LocalDiskStorage.SaveDir` / `URLBase` fields **removed** — fields are unexported. Construct via `builtin.NewLocalDiskStorage(saveDir, urlBase)`.
- The five async-task tools' exported `Manager` field is now unexported. Construct via the new `New…` functions.

## [v0.16.0] — 2026-05-04

Adopter-blocking fixes batch — cloud deployments work, hooks have typed payloads, wrapper traps documented at the source.

### Added
- `tools.AssetStorage` interface + `tools.LocalDiskStorage` default implementation. `GenerateImageTool` and `GenerateVideoTool` accept any `AssetStorage` so cloud / container adopters plug in GCS / S3 / Azure Blob without writing wrappers. Local-disk path stays a one-liner via `&builtin.LocalDiskStorage{SaveDir: ..., URLBase: ...}`. (#77)
- `tools.StructuredResult` interface — tools opt in via `ExecuteStructured(ctx, args) (string, any, error)` to expose a typed payload alongside the LLM-facing string. `ToolResultHook` gains a `structured any` parameter so post-execution hooks can read typed fields instead of regex-parsing markdown. (#78)
- Wrapper-trap doc warnings on `tools.InlineRenderer` and `tools.Cacheable` interface declarations — making the silent-method-drop bug visible at the godoc site. Includes the explicit forwarding pattern. (#76)

### Changed (breaking)
- `GenerateImageTool.SaveDir` / `URLBase` and `GenerateVideoTool.SaveDir` / `URLBase` fields **removed**. Replace with `.Storage = &builtin.LocalDiskStorage{SaveDir: ..., URLBase: ...}`.
- `ToolResultHook` signature gains `structured any` as the 5th parameter. Existing hooks need a `_ any` placeholder.

## [v0.15.0] — 2026-05-04

Adopter quality-of-life batch — multimodal entry, retry observability, cap unification, silent-failure fixes.

### Added
- `AgentLoop.RunIterationStreamMessage(ctx, sessionKey, msg, ch)` and `RunIterationMessage(ctx, sessionKey, msg) (string, error)` accept a full `history.Message` instead of a plain string. Adopters can now pass user messages with multimodal `Parts` (image bytes, mixed text+image), `ToolCallID`, and `CacheHint` directly at chat time. The string-based `RunIteration{Stream}` keeps working — they wrap into a default-role `"user"` message internally. (#72)
- `EventTypeLimitExhausted` / `LimitExhaustedEvent { Kind, Limit, Used }` — unified typed signal for cap trips. Emitted on `MaxIters`, `MaxToolCallsPerSession`, and Anthropic `stop_reason="max_tokens"`. Adopters can render user-friendly messages via a single switch on `Kind` instead of parsing strings or reading multiple code paths. Visitor gains `VisitLimitExhausted`. `MaxItersReachedEvent` keeps firing alongside for back-compat. (#70)
- `llm.WithMaxTokens(n)` constructor option for `NewAnthropicProvider`. The default also bumps from 4096 → 8192 (`DefaultAnthropicMaxTokens`) so code-gen turns (HTML5 playables, full-schema dumps, inline charts) stop truncating silently. Anthropic `stop_reason="max_tokens"` now surfaces as `LimitExhaustedEvent{Kind: "provider_max_tokens"}`. (#70)
- `RetryConfig.OnAttempt RetryAttemptHook` — fires once per retry attempt with structured `(attempt, err, nextDelay)` so adopters can answer "is this turn slow because of retries or one slow call?" without parsing thought events. Nil hook is zero-cost; default behavior unchanged. (#71)
- `history.Message.IsInlineResult bool` — set automatically on `role:"tool"` rows whose tool implements `tools.InlineRenderer` with `InlineResult()=true`. Render layers can now special-case inline-result rows on session resume (fold them back into the preceding assistant message) instead of maintaining a hand-curated tool-name allowlist. Failing tools are never flagged. (#73)

### Changed (breaking)
- `EventVisitor` interface gains `VisitLimitExhausted`. Custom visitors must add the method.

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

[v0.23.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.23.0
[v0.22.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.22.0
[v0.21.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.21.0
[v0.20.2]: https://github.com/hung12ct/gopheragent/releases/tag/v0.20.2
[v0.20.1]: https://github.com/hung12ct/gopheragent/releases/tag/v0.20.1
[v0.20.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.20.0
[v0.19.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.19.0
[v0.18.1]: https://github.com/hung12ct/gopheragent/releases/tag/v0.18.1
[v0.18.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.18.0
[v0.17.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.17.0
[v0.16.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.16.0
[v0.15.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.15.0
[v0.14.2]: https://github.com/hung12ct/gopheragent/releases/tag/v0.14.2
[v0.14.1]: https://github.com/hung12ct/gopheragent/releases/tag/v0.14.1
[v0.14.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.14.0
[v0.13.0]: https://github.com/hung12ct/gopheragent/releases/tag/v0.13.0
