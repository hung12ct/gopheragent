# Long conversations & context size

Stored history is never rewritten by the loop. Before every LLM call the loop
derives the request from it, and every way it shrinks that request is reported
on a `context_trace` event. There are four layers, from cheapest to most
thorough.

| Layer | Option | What it does |
|---|---|---|
| Payload pruning | always on | Cuts the middle out of long tool/assistant messages; discards outliers |
| Token budget | `WithMaxTokenBudget(n)` | Near the budget, truncates tool results; over it, prunes harder |
| History window | `WithHistoryWindow(n)` | Sends only about the last `n` messages |
| Compaction | `WithCompactor(c)` + window | Folds the turns the window drops into a running summary |

Payload pruning and the budget keep every message, so a session that lives
for months still grows its request with every turn. The window and compaction
are what keep it flat.

## History window

```go
loop := agent.NewAgentLoop(sm, reg, provider,
    agent.WithHistoryWindow(80),
)
```

Each request carries the leading system messages plus roughly the last 80
messages. The cut:

- always starts at a user message, so a tool call is never separated from its
  result, and a single turn longer than the window is kept whole;
- advances in steps of `n/2`, not one message per turn, so the request prefix
  stays byte-identical across many turns and provider prompt caching keeps
  hitting. The request holds between `n/2` and `n` messages.

Without a compactor, the dropped turns are simply not sent. They stay in
`Sessions.History`, and the memory consolidator still reads them.

## Compaction

```go
loop := agent.NewAgentLoop(sm, reg, provider,
    agent.WithHistoryWindow(80),
    agent.WithCompactor(&agent.Compactor{LLM: cheapProvider}),
)
```

At the start of a Run whose conversation has outgrown the window, the
compactor makes one LLM call. It merges the previous summary with every
message except the last `n/2`, and inserts the result into history as a
compaction record (a `role:"system"` message with `Compaction` set) right
before the first message it keeps. Later requests carry the summary inside the
system prompt in place of everything before the record. Without
`WithHistoryWindow`, a window of 100 messages applies.

- **Incremental and bounded.** Each call reads the previous summary plus about
  `n/2` messages, each clipped to 2,000 characters. Input is capped at about
  100k characters. That cap only matters on the first compaction of a session
  that predates the compactor; it keeps the newest messages and reports the
  rest as `CompactedEvent.Omitted`. The summary is capped by
  `MaxSummaryRunes` (default 6,000).
- **Lossless, position-based storage.** The summarized messages stay in
  history. A record covers everything before it, so no index is stored.
  Trimming history from the front, or truncating it before the record, cannot
  make it point at the wrong place.
- **Cache-friendly.** The system prompt plus summary only changes when a new
  record lands, about once every `n/2` messages.
- **Never fails a turn.** A failed or empty summary is reported on
  `CompactedEvent.Error`, no record is written, the window still bounds the
  request, and the next Run retries.
- **One system message.** The summary is folded into the head system message,
  never sent as a second one, because some providers keep only one. It is
  framed as a record of what was said, not instructions, and cannot close its
  own frame.
- **Billable.** The summarizer runs on the compactor's own LLM, so its token
  usage is reported on `CompactedEvent.Usage` and not in the turn's usage.

The default prompt keeps goals and their status, established facts and
decisions, open threads, exact identifiers (names, IDs, paths, amounts) and the
user's stated preferences. It writes in the conversation's language. Override
it with `Compactor.Prompt`; `{MAX_RUNES}` is replaced with the cap.

Compaction and the memory consolidator (`WithMemoryConsolidator`) do different
jobs. The summary keeps the thread of *this* conversation. The consolidator's
notes keep durable facts about the user across sessions.

### Rendering history

A UI that lists `Sessions.History` will see compaction records as system
messages. Skip them along with other system rows, or render
`msg.Compaction != nil` as a "conversation condensed" divider.

## Tracing what was left out

`context_trace` reports `WindowStart` / `WindowDropped` when the window cut
messages, and `SummaryIndex` (the record's stored index) when the request
carried a summary.
`ContextRef.Index` always indexes stored history, so you can join a trace back
to `Sessions.History` even after a cut.
