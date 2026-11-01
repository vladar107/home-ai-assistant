# 0128 — "Why did you say that?": what a reply was based on

Status: `done`

## Context

A Telegram reply maps back to its request and session through
`conversation_reply_messages`, but nothing records what the turn did. Tool
calls are counted per turn and per day, and traced and logged without
arguments, results, or a turn ID. Only the Pi session file holds the full
calls, and it is subject to session retention (plan 0102).

When a reply surprises the user, neither they nor the operator can see which
tools ran, which sources were read, or what the model assumed.

## Goal

- The user can send `/why` in reply to any assistant message, or reply "how
  do you know?", and get a short account:
  - which tools ran, in order, and with what outcome;
  - which sources were read, each a short label;
  - whether outside content was involved;
  - which model and effort answered;
  - an explanation of the reasoning and any assumptions, in a few sentences.
- This works for chat replies, scheduled reports, background task results,
  and watch alerts.
- The record is kept 30 days and holds no tool results.

## Decisions

- **A per-turn trace, recorded by the turn engine.** Migration 052 adds:
  - `turn_traces`: principal, trace key, kind, model, effort, started and
    finished times, flags for outside and private content, and a detail
    that says what code checked for a message no model turn wrote.
  - `turn_trace_calls`: principal, trace key, sequence, call ID, tool,
    outcome, duration, result size, and source label.
  - `turn_messages`: principal, chat ID, Telegram message ID, and trace key,
    for every message a traced turn delivered. It is pruned with the traces,
    and a key without a trace, such as a reminder's run, links nothing.
  - a `trace_key` column on `watch_alerts`, carrying an agent check's key
    from the check to the alert's delivery.

  The trace is written from the turn engine's event sink, the one that
  counts calls today, when the run ends. That sink also sees the runtime's
  own web search, which never passes through the broker. Runs that share a
  key add their calls to one trace, after the earlier ones. A chat turn's
  trace is written beside its reply rather than before it: the engine holds
  the reply's link to the trace until the write ends, and waits for writes
  still on their way at shutdown.

  Today `runtime.Event` carries only the tool name for a call, with no call
  ID, arguments, or error flag. So:
  - Pi's `tool_execution_start` and `tool_execution_end` events are mapped
    with their `toolCallId`, their arguments (on start), and `isError` (on
    end); an end without a result is reported too.
  - The bridge extension sends the call ID with each call, and the broker
    keeps the call's outcome and label under it in the run's `tools.CallLog`
    (plan 0121), extended for this. The executor keeps a log of its own when
    the caller attached none, so parallel calls match their labels exactly.
    The broker's outcome, which is finer, replaces the runtime's error flag
    when both exist.
  - Arguments are used only to derive labels in memory and are never
    stored.
- **Trace keys.** Not every turn has a stable key today, so each kind gets
  one:
  - Chat turns use the request's turn key.
  - Scheduled runs use the run's turn key. The scheduler puts it on the
    context of the whole run, so a job's condition check, the inbox and
    trips scans, and project nudges, which are scheduled turns, add to the
    run's trace.
  - Background tasks get a key per start of the task, which its report run
    shares, so a report is explained from everything that start did.
    `TaskTurn` names the task, not the start. The report's messages are now
    sent tracked and recorded in `turn_messages`.
  - Agent watch checks get a check key, recorded with the alert they
    produce. They run today with an empty turn key. A check that cannot get
    a key fails, so an agent alert is never explained as code-written.
  - The `RunAssess` turn of plan 0127 is traced under its event's key and
    marked as having read outside content, since the event's text is its
    message. A delivered event is linked to it only when the assessment
    decided on the event; one forwarded after a failed assessment is not.
    `RunReview` delivers no message of its own, so it is not traced.
  - Code-checked watch alerts, and the failure notice of any watch, have no
    model turn. At delivery they get a trace of kind `code` whose detail is
    the watch's label and rules as `Describe` words them; "why?" on one
    renders that, with no explanation turn.
  - Traces of checks and assessments that raised no message are pruned after
    2 days rather than 30, since most checks alert nothing.

  `job_reports` is trimmed to 200 per principal and is not relied on.
  `turn_messages` is the link.
- **Source labels, not arguments or results.** A tool definition may carry
  an optional `SourceLabel(arguments, result)` function, given the
  provider's own result, that returns a short label; the broker cleans it to
  one line of at most 120 runes. Helpers label by argument, by a URL's host
  and path, or by a result line. Labels so far:
  - a URL's host and path for a page fetch;
  - a sender and subject for a mail read, a query for a mail search;
  - an entity's name or area for Home Assistant;
  - a page title for a Notion page read, a query or ID otherwise;
  - a file path for the NAS;
  - an event's title for Calendar, a query for Grafana, stations and stops
    for transit, the place for weather;
  - IDs and names for memory, procedures, collections, projects, intake,
    events, charts, places, and conversation search.

  The runtime's web search is labelled with its query, taken from the event
  stream. Tools without a label record only their name. Labels can be
  private, so they stay in the owner's database and are never logged,
  exported to telemetry, or put on a span.
- **The explanation is one extra turn, only on request.** Recording the
  trace costs nothing at reply time. On "why?", the assistant runs a short
  turn without tools, memory, or web search on the owner's subscription and
  the runtime's default model, in the background so the Telegram poller
  never waits on it. Each principal has at most one being written; another
  question meanwhile is told to wait. The user waits on it, so it runs as a
  chat turn and neither queues behind nor takes the shared background slots;
  with one per principal it holds no more of the process pool than a chat
  turn. The record lists the first 20 calls and counts the rest, and the
  answer is split like any reply, its first part quoting the question. It
  gets:
  - the code-rendered trace;
  - the replied message's text;
  - for a chat reply, the user's messages before it: the last two from
    `turnstate` while the tab's latest reply is the message asked about, or
    otherwise the one before the reply in the conversation's history (Pi's
    session file). A request's own text is cleared once its turn is done, so
    it cannot be read from the request.

  It explains in the user's language, and must not invent calls missing
  from the trace. The tool list and source labels are rendered by code
  above the explanation, so the facts never depend on the model, and a line
  says the explanation is a reconstruction.
- **Trigger.** `/why` in reply to a message, or a short reply from a fixed
  list of questions about a message's sources ("how do you know?", "where is
  that from?", "what is that based on?", "source?", and their Russian and
  Dutch equivalents), whatever their case or the punctuation and emoji after
  them; no existing intent handling recognises such replies. A bare "why?"
  asks about the content as often as about its sources, so it goes to the
  conversation as before, and `/why` stays the explicit way. A message
  without a trace, such as one older than 30 days or from before this plan,
  gets "I no longer have the record of that reply." for `/why`; a short
  reply to one goes on to the conversation, which can answer it with the message as
  context. `/why` without a reply says how to use it.
- **Retention and privacy.** Traces are pruned after 30 days by the
  scheduler's hourly pruning. A trace is found by principal, chat, and
  message ID, so another principal's traces are never readable.
- **A package of its own.** The lookup, rendering, and explanation live in
  `internal/provenance`, which Telegram calls; the prompt is in
  `internal/prompt`.

## Approach

1. Migration, store, the trace recorder in the turn engine's sink, label
   hand-off from `internal/tools/broker.go`, and pruning.
2. Source labels for web, Gmail, Calendar, Notion, NAS, Home Assistant,
   Grafana, transit, and memory tools, and for runtime web searches.
3. Trace keys for task runs and agent watch checks, and `turn_messages`
   rows for chat, scheduled, task, and watch deliveries.
4. The `/why` command, reply recognition, the code-rendered trace, and the
   explanation turn.
5. README, `docs/architecture.md`, and `docs/operations.md`.

## Files affected

- `internal/store/sqlite/migrations/052_turn_traces.sql`, a new store file,
  `watches.go` (the alert's trace key)
- `internal/runtime` (`Event` and `ToolCall` gain call ID, arguments, and
  error flag), `internal/runtime/pi` (event mapping, bridge call ID),
  `internal/testutil/pifixture`
- `internal/turn` (`engine.go` sink, `scheduled.go`, `task.go`, a new
  `trace.go`), `internal/tools` (`CallLog`, labels), the tool definitions of
  the connectors and of collections, projects, intake, events, memory,
  charts, and conversation search, `internal/outbox` (per-start task key)
- `internal/conversation`, `internal/schedule`, `internal/task`,
  `internal/watch`, `internal/events` (turn keys and message links)
- `internal/provenance` (new), `internal/prompt` (the explanation's prompt)
- `internal/telegram` (command and reply recognition), `internal/wiring`,
  `internal/app`, README, docs

## Validation

- Integration tests with the fake model runtime:
  - a turn with three tool calls records them in order with labels;
  - `/why` on its reply shows them and the explanation;
  - a scheduled report, a task result, and an agent watch alert can be
    explained;
  - a code-checked watch alert is explained from its spec without a model
    turn;
  - a runtime web search appears in the trace with its query;
  - two parallel tool calls each get their own label and outcome, matched by
    call ID;
  - a failed call is recorded as failed from the runtime's error flag;
  - a message without a trace gets the "no record" answer;
  - a trace older than 30 days is pruned;
  - labels never reach logs or span attributes;
  - another principal's message cannot be explained;
  - a trace of 100 calls is answered with the first 20 listed, the rest
    counted, and the answer split;
  - an assessed event is explained as outside content, and one forwarded
    after a failed assessment has no record;
  - a second question while an answer is written is told to wait, and a
    bare "why?" reaches the conversation;
  - the phrase matcher ignores case, spacing, and trailing punctuation and
    emoji, and leaves a bare "why?" alone.
- A live check from the GoLand server: ask something that needs a web search,
  then `/why`.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- The explanation of reasoning is the model's reconstruction, not its actual
  chain of thought. The code-rendered part is the reliable record, and the
  reply says so in one line.
- Writing a few rows per turn adds a small amount of SQLite traffic,
  negligible next to existing turn writes.
- Traces are operational records and are not exported by plan 0129.
