# 0102 — Pi runtime limits, session retention, and photo privacy

Status: `done`

## Context

Every model turn starts its own Pi process, a Node program, and nothing
limits how many run at once. The container has 1 GB of memory, one CPU, and
256 processes. Chat lanes, scheduled runs, background tasks, tab naming,
alert rewrites, and agent-watch checks all start Pi in parallel, so a
morning peak can reach six or seven Node processes. Several cold starts on
one CPU can also take long enough to trip the 45-second stall detector. The
observed peak is far below the limit, so this change prevents a problem
rather than fixing one.

Pi gets the system prompt on its command line, and with it the user's memory
and job instructions. Any user on the host can read a process's command line.

Pi's stderr is thrown away, so when Pi exits early the log does not say why.

Chat session files are never removed. A conversation closes only on `/new`,
a switch, or a closed tab, and the store keeps every conversation row. The
pruner keeps every file a row refers to. Pi reads and parses the whole
session file on every start, so startup time, Node heap, and disk use grow
without bound. Photos make files grow fastest. Pi 1.0.0 puts a prompt's
images into the user message as base64 `image` content blocks, and
`SessionManager.appendMessage` writes that message to the session file.
`/privacy` says "Photos and files you send are kept until /new", but `/new`
deletes only the application's copies. The Pi session file keeps the photo.

## Goal

- No more than a fixed number of Pi processes run at once. Chat turns, which
  a user is waiting on, always have capacity of their own. Time spent
  waiting for a slot respects cancellation and does not count as a stall. A
  gauge shows how many processes run and how many wait, by kind.
- Memory and job instructions never appear on a command line.
- When Pi exits early, the log shows the cause from the end of its stderr,
  with secrets scrubbed out.
- Closed conversations stay resumable for a retention period, and then
  they and their Pi files are deleted.
- The `/privacy` promise is true. When a conversation ends, its photos are
  removed from the Pi session file as well as from our own copies.

## Decisions

- **Limiter in the runtime, with the kind on `RunRequest`.** The new field
  `RunRequest.Kind` takes one of three values:
  - `RunChat`: a chat turn.
  - `RunTask`: a background task, which can run for an hour.
  - `RunBackground`: everything else, such as scheduled runs, checks, tab
    names, and rewrites.

  `RunBackground` is the zero value, so a new caller never takes a chat
  turn's place. At most 5 Pi processes run at once. Tasks and other
  background work may each hold at most 2 slots. A chat turn therefore
  always gets a slot once one ends, and an hour-long task never holds up
  morning runs, or the other way round. When a slot frees up, waiting chat
  turns get it first, then tasks, then other background work, first come
  first served within each kind. A turn takes its slot before it starts its
  bridge or process and keeps it through the stall retry. The stall timer
  starts only once the process runs. The limits are constants, not
  settings. An early draft with only two kinds was dropped: two users'
  tasks would have taken the whole background share for an hour.
- **Gauge.** `home_agent.model.processes` is an up-down counter with
  `home_agent.kind` (`chat`, `task`, `background`) and `home_agent.state`
  (`running`, `waiting`). It goes through the runtime's existing telemetry
  interface, so no other wiring is needed.
- **System prompt through a file.** Pi 1.0.0's `--system-prompt` reads a
  path when the path exists (`resolvePromptInput` in
  `core/resource-loader.js`). Each turn writes the prompt to a `0600` file
  in its own `turn-*` directory under the principal's `0700` agent
  directory, passes the path, and removes the directory when the turn ends.
  Start-up removes `turn-*` directories left by a killed process, as it
  already does for `bridge-*`.
- **Stderr tail.** The last 4 KiB of Pi's stderr is kept. When Pi exits or
  closes its output before the turn completes, the log entry
  `pi.process.exited_early` records the exit code and the scrubbed tail.
  The tail is cut to 1 KiB, and bearer and authorization values, key, token,
  secret, and password assignments, JWTs, known key prefixes, email
  addresses, and long opaque runs are redacted. The returned error gives
  only the exit code. Errors travel further than the log, and provider text
  is kept out of errors, as `ProviderError` already does.
- **No idle rotation (considered, declined by the operator).** We
  considered starting a new conversation in the main view after 6 quiet
  hours, so an active session file would stop growing. The operator
  declined it. A conversation still closes only on `/new`, a switch, or a
  closed tab. A long-lived main-view session keeps growing, but stripping
  photos on close and the retention below bound everything else.
- **Retention of closed conversations: 90 days after they close.** The
  scheduler's hourly session prune first deletes the conversations that
  closed more than 90 days ago. Requests, reply messages, feedback, and
  attachment rows go with them through the existing cascades. Their Pi
  files are then unreferenced and older than the 30-day cutoff for run
  files, so the same pass deletes them. Until then, a closed conversation
  can be resumed in `/sessions` and searched by the history tools.
- **Strip photos when a conversation ends (keep the promise).** The
  runtime's `ForgetImages` rewrites the session's files and replaces every
  `image` content block with a short text placeholder, so the conversation
  can still be resumed with its text. A file with no image blocks is not
  rewritten. A rewritten file keeps its modification time, so retention
  does not restart. The rewrite goes to a temporary file in the same
  directory and is renamed over the original. It is called wherever our
  attachment copies are removed: `/new`, a switch (including
  `/sessions`), and a tab the user closed. A gone tab can come back, so
  its conversation keeps everything, as before.
- **Session gate.** A turn holds its session's gate for the whole run, so
  a rewrite never races Pi's appends or a stall restore. When
  `ForgetImages` finds the session busy, it marks the session, and the turn
  strips the images before it releases the gate. The hourly prune is the
  backstop after a crash. It strips the closed conversations' files that
  changed since its previous pass, or all of them on its first pass after a
  start.
- **Privacy text.** `/privacy` now says when photos and files are deleted,
  including from the conversation history.

## Approach

1. Runtime: process limiter and gauge, `Kind` on `RunRequest`, the
   system prompt file, the stderr tail, the session gate, `ForgetImages`,
   and a stateful pruner that deletes expired conversations, strips the
   closed ones, and removes unreferenced old files.
2. Store: `ReferencedSessions` reports whether each referenced conversation
   is closed, plus `DeleteClosedConversations` and
   `ConversationRuntimeSession`.
3. Conversation: give chat turns and tasks their kind, and forget a
   conversation's photos wherever its attachments are closed, through the
   new optional `Config.Transcripts`.
4. Telegram: the `/privacy` line.
5. App: pass the runtime as `Transcripts`.
6. Pi fixture: read the system prompt from its file, record image blocks the
   way Pi does, and add a scenario that writes to stderr and exits.
7. Docs: architecture (earlier conversations, retention, photos) and
   operations (process limit, gauge, early-exit log).

## Files affected

- `internal/runtime/runtime.go`, `internal/runtime/pi/` (runtime, limiter,
  transcripts, tests)
- `internal/testutil/pifixture/fixture.go`
- `internal/observability/telemetry.go`
- `internal/store/sqlite/conversation.go`,
  `internal/store/sqlite/conversation_history.go` and tests
- `internal/conversation/service.go`, `internal/conversation/task.go`,
  `internal/conversation/types.go`
- `internal/telegram/commands.go` and the integration harness
- `internal/app/app.go`
- `docs/architecture.md`, `docs/operations.md`

## Validation

- Runtime tests with the Pi fixture:
  - the limiter caps background work and lets a chat turn through;
  - a waiting turn ends on cancellation;
  - the prompt reaches Pi from a `0600` file and not from argv, and the
    turn directory is removed afterwards;
  - an early exit logs a scrubbed stderr tail;
  - `ForgetImages` strips image blocks and keeps text, and a busy session
    is stripped when its turn ends;
  - the pruner deletes expired conversations' files and strips closed ones.
- Store tests for retention cascades and closed flags.
- Conversation and Telegram tests:
  - `/new` strips a photo from the Pi file;
  - the updated `/privacy` text.
- Full check suite.

## Risks and open questions

- Fixed limits: 5 processes in total, and at most 2 each for tasks and for
  other background work. Raise them only if the gauge shows chat turns
  waiting while memory stays well below the container limit.
- The operator chose 90 days of retention. The history tools cannot find a
  conversation once it has been deleted, and durable facts belong in memory.
- With no idle rotation, a main-view conversation the user never ends with
  `/new` keeps growing, and so does Pi's startup cost for it. If that
  becomes visible in turn latency, idle rotation is the follow-up to
  revisit.
- Stripping changes how a resumed conversation looks to the model: the
  photo becomes a placeholder. This matches the attachment copies, which
  are already gone at that point.
- The stderr tail can hold text Pi printed about the turn. It is logged
  only on an early exit, kept short, and scrubbed.
- Dashboards in `home-monitoring` can add the new gauge separately.
- Two first turns of a new principal that start together both try to
  create the session directory, and one fails. The limiter does not change
  this. It happened before and is left as it is.
