# 0128 — "Why did you say that?": what a reply was based on

Status: `proposed`

## Context

A Telegram reply maps back to its request and session through
`conversation_reply_messages`, but nothing records what the turn did. Tool
calls are counted per turn and per day, and traced and logged without
arguments, results, or a turn ID. Only the Pi session file holds the full
calls, and it is subject to session retention (plan 0102).

When a reply surprises the user, neither they nor the operator can see which
tools ran, which sources were read, or what the model assumed.

## Goal

- The user can reply "why?" to any assistant message, or send `/why` in reply
  to it, and get a short account:
  - which tools ran, in order, and with what outcome;
  - which sources were read, each a short label;
  - whether outside content was involved;
  - which model and effort answered;
  - an explanation of the reasoning and any assumptions, in a few sentences.
- This works for chat replies, scheduled reports, background task results,
  and watch alerts.
- The record is kept 30 days and holds no tool results.

## Decisions

- **A per-turn trace, recorded by the turn engine.** A new migration adds:
  - `turn_traces`: principal, trace key, kind, model, effort, started and
    finished times, and flags for outside and private content.
  - `turn_trace_calls`: principal, trace key, sequence, call ID, tool,
    outcome, duration, result size, and source label.
  - `turn_messages`: principal, chat ID, Telegram message ID, and trace key,
    for every message a traced turn delivered. It is pruned with the traces.

  The trace is written from the turn engine's event sink, the one that
  counts calls today. That sink also sees the runtime's own web search,
  which never passes through the broker. The broker contributes the source
  label of each brokered call.

  Today `runtime.Event` carries only the tool name for a call, with no call
  ID, arguments, or error flag. So:
  - Pi's `tool_execution_start` and `tool_execution_end` events are mapped
    with their `toolCallId`, their arguments (on start), and `isError` (on
    end).
  - The broker reports its label under the same call ID, so parallel calls
    match their labels exactly.
  - Arguments are used only to derive labels in memory and are never
    stored.
- **Trace keys.** Not every turn has a stable key today, so each kind gets
  one:
  - Chat turns use the request ID.
  - Scheduled runs use the run ID.
  - Task runs get a per-run key. `TaskTurn` names the task, not the run, and
    task result messages are linked nowhere yet. The result message is now
    recorded in `turn_messages`.
  - Agent watch checks get a check key, recorded with the alert they
    produce. They run today with an empty turn key.
  - Turns run without a sink today (`RunReview` discards its events), and
    the `RunAssess` turn of plan 0127 is traced too, under its event's key.
  - Code-checked watch alerts have no model turn. "Why?" on one answers from
    the watch spec in code: "checked by code: departure 08:12, delay over 5
    minutes", with no explanation turn.

  `job_reports` is trimmed to 200 per principal and is not relied on.
  `turn_messages` is the link.
- **Source labels, not arguments or results.** Each tool may implement an
  optional `SourceLabel(arguments, result)` that returns a short, safe label.
  Examples:
  - a URL's host and path for a page fetch;
  - a sender and subject for a mail read;
  - an entity's friendly name for Home Assistant;
  - a page title for Notion;
  - a file path for the NAS.

  The runtime's web search is labelled with its query, taken from the event
  stream. Tools without a label record only their name. Labels are capped at
  120 runes.
  They can be private, so they stay in the owner's database and are never
  logged or exported to telemetry.
- **The explanation is one extra turn, only on request.** Recording the
  trace costs nothing at reply time. On "why?", the assistant runs a short
  turn without tools on the owner's subscription. It gets:
  - the trace;
  - the replied message's text;
  - the turn's last two user messages, from `turnstate` if still there, or
    from the request otherwise.

  It explains in the user's language, and must not invent calls missing
  from the trace. The tool list and source labels are rendered by code
  above the explanation, so the facts never depend on the model.
- **Trigger.** `/why` in reply to a message, or a short reply recognised by
  the existing intent handling ("why?", "how do you know?"). A message
  without a trace, such as one older than 30 days or from before this plan,
  gets "I no longer have the record of that reply."
- **Retention and privacy.** Traces are pruned after 30 days by the existing
  pruning loop. Another principal's traces are never readable.

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

- `internal/store/sqlite/migrations/NNN_turn_traces.sql`, a new store file
- `internal/runtime` (`Event` gains call ID, arguments, and error flag),
  `internal/runtime/pi` (event mapping)
- `internal/turn` (`engine.go` sink, `scheduled.go`), `internal/tools`, the
  connectors' tool definitions, `internal/outbox` (per-run task key)
- `internal/conversation`, `internal/schedule`, `internal/task`,
  `internal/watch` (turn keys)
- `internal/telegram` (command), `internal/wiring`, README, docs

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
  - another principal's message cannot be explained.
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
