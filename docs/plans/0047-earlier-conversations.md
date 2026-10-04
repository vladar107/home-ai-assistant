# 0047 — Return to earlier conversations

Status: `done`

## Context

Each principal has exactly one active conversation session; `/new` closes it,
and nothing can reopen it. The closed session keeps its `runtime_session_id`,
and Pi keeps that session's JSONL file in the principal's `sessions` directory
indefinitely, so the full history still exists. SQLite deliberately clears
message and reply text after delivery, so Pi's files are the only copy. The
model cannot see an earlier session either, so "what did we decide about the
heating last week?" fails after any `/new`.

Two further gaps:

- When a restart interrupts a turn, `RecoverConversations` gives the active
  session a new runtime session id. The conversation continues in a fresh Pi
  file and the earlier context is lost.
- `SendReply` does not return the sent message's id, so the bot cannot tell
  which session a reply belonged to.

Usage is small: about 200 chat turns a month across both users.

## Goal

A user can list their recent conversations and resume one. Replying in Telegram
to an answer from an earlier conversation continues that conversation. The model
can search the user's own earlier conversations and read a bounded part of one
when the user refers to something discussed before.

## Decisions

- **Titles are read from Pi's files when listing, not stored.** The first user
  message of a session, without the `[system]` lines the application adds and
  the attached-file list it appends, on one line and cut to 60 characters, is
  its title. No message text is copied into SQLite, so the current privacy
  stance holds.
- **`/sessions` lists the last ten sessions** by last activity (the newest
  request a session took), newest first, each with its date, title, and a
  Resume button. The current session is listed and marked instead of offered;
  an earlier session in whose files Pi recorded nothing the user wrote is left
  out. Resuming closes the active session with the reason `switched` and
  reactivates the chosen one; the next turn resumes its Pi file. A pending turn
  blocks the switch with the busy reply. The press refreshes the list in place,
  like `/jobs`, and a separate message names the conversation.
- **Leaving a conversation releases its files.** A switch closes the left
  session's attachments as `/new` does, which the start-up purge would do for a
  closed session anyway; the privacy text already promises that files last only
  for the current conversation.
- **Reply to continue.** Delivered replies record their Telegram message id with
  the session id in `conversation_reply_messages`. A user message that replies
  to a recorded message of a closed session switches to that session first, and
  the answer is prefixed with "Back in the conversation from <date>." A reply to
  a message of the active session behaves as today. The switch happens in the
  enqueue transaction, so a busy reply rolls it back, and the lookup is scoped
  to the sender, so a reply to another principal's message changes nothing.
  The date is the resumed session's last activity, stored on the request as
  `resumed_from` so a restart before delivery keeps the note. The bot's own
  quoting of the request (plan 0006) is unchanged.
- **`SendReply` keeps its signature.** The client gains `SendTrackedReply`,
  which returns the message id, and only conversation delivery uses it; the
  other dozen callers stay as they are. Only text chunks are recorded: a day
  card photo is not, so replying to a card does not switch. A failure to record
  an id is logged and does not fail the delivered reply.
- **The model gets two read tools**, both scoped to the calling principal:
  - `conversation_search` takes words and returns up to eight matches, each
    with the session date, its id, and a short snippet.
  - `conversation_read` takes a session id and a match position and returns a
    bounded window of user and assistant text around it.
  Both read only user and assistant messages from the principal's own session
  files: no tool results, no system lines, and nothing from another principal's
  directory. The broker already marks every result as untrusted data.
  Details as built:
  - Only sessions recorded in `conversation_sessions` for the principal are
    reachable, the 200 most recently active, so scheduled-run sessions and any
    other Pi files are out of scope. The id is the conversation session id,
    resolved to its runtime id through SQLite.
  - A match needs every query word (up to eight, case-insensitive) in one
    message; at most three matches per session, newest sessions first. The
    snippet is 200 characters around the first word. The current conversation
    can match and is labelled, except its last user message, which is the
    question being answered.
  - A read returns up to three messages either side, each cut to 1,500
    characters, within 12 KiB, under the broker's 16 KiB limit.
  - Errored or aborted assistant messages, thinking, and tool calls are
    skipped. A session spread over several files, as pre-0015 sessions are, is
    read across them in file name order.
  - Files over 32 MiB are read only up to that size, and single entries over
    4 MiB, such as a message carrying a large photo, are skipped.
  - The tools are offered once the principal has a conversation besides the
    current one.
- **No full-text index.** At this volume a scan of the principal's JSONL files
  per search is fast enough and avoids a second copy of the history. An index
  becomes a decision only if searches get slow.
- **An interrupted turn no longer starts a fresh Pi file.** Recovery keeps the
  runtime session id. Checked against Pi 1.0.0's code (no model call):
  - `--session-id` resolves through `SessionManager.findById`, which matches the
    header id and the working directory, then `SessionManager.open` loads every
    entry and sets the leaf to the last one, with no check of its role
    (`dist/core/session-manager.js`, `_setSessionFile`, `_buildIndex`).
  - The new prompt is appended as a child of that leaf, so the context holds
    two user messages in a row. `transformMessages` in
    `pi-ai/dist/api/transform-messages.js` passes consecutive user messages
    through, drops errored and aborted assistant messages, and inserts
    synthetic "No result provided" results for tool calls that never got one,
    which covers a turn cut off mid-tool.
  - A line truncated by a kill is skipped by `loadEntriesFromFile`, which also
    repairs the file. A file Pi cannot match is not found, and Pi then starts a
    new file under the same id, so rotating the id in SQLite adds nothing.
- `/privacy` says that earlier conversations stay on the NAS until deleted and
  can be resumed and searched by their owner's assistant only. The sentence is
  part of the conversation privacy reply rather than an optional line, since it
  applies whenever conversation is on.
- **Migration 015 rebuilds the conversation tables.** SQLite cannot widen the
  `close_reason` check in place, so `conversation_sessions` is rebuilt with its
  dependants, `conversation_requests` (which gains `resumed_from`) and
  `conversation_attachments`, the way migration 011 rebuilt the schedule
  tables. The rebuilt sessions table keeps the `exposed_at` column that plan
  0046 added.
- The history tools are offered in chat turns only, not to scheduled runs or
  background tasks, which are told they cannot see the chat. Their results are
  marked third-party, because an earlier conversation may retell mail or pages
  it read, so reading one makes later memory writes in the conversation need
  Confirm.
- The Pi test fixture now records each plain exchange in a Pi-format session
  file, resumed only from the same working directory, so integration tests read
  real session files through the real adapter.
- Telemetry: `home_agent.conversation.resumes` by trigger (`command`, `reply`)
  and `home_agent.conversation.searches` by outcome (`found`, `empty`,
  `invalid`, `failed`), without content.

## Approach

1. Check Pi 1.0.0 against a session file that ends in an unanswered user
   message, then change recovery to keep the session id if Pi handles it.
   Complexity: simple.
2. Migration `015_conversation_history.sql`: the `switched` close reason and
   `conversation_reply_messages`. `SendReply` returns the message id; delivery
   records it. Complexity: simple.
3. `internal/conversation/history`: read a principal's session files for
   titles, search, and bounded reads, with size limits per file and per result;
   the tool provider for the broker. Complexity: moderate.
4. Resume: store method that switches the active session atomically; the
   conversation service's switch path; `/sessions` with Resume callbacks; reply
   routing in the Telegram poller. Complexity: moderate.
5. Telemetry: resumes by trigger (command, reply) and searches by outcome.
6. Update `README.md`, `docs/architecture.md`, `/help`, and `/privacy` text.

## Files affected

- `internal/store/sqlite/migrations/015_conversation_history.sql`,
  `internal/store/sqlite/conversation.go`,
  `internal/store/sqlite/conversation_history.go`
- `internal/conversation/` (switching, recovery, history reader in `history/`)
- `internal/runtime/pi/` (session file location)
- `internal/telegram/client.go`, `internal/telegram/service.go`,
  `internal/telegram/commands.go`, `internal/telegram/sessions.go`
- `internal/observability/telemetry.go`
- `internal/testutil/pifixture/fixture.go`
- `internal/app/app.go`
- `README.md`, `docs/architecture.md`

## Validation

- `gofmt`, `go vet ./...`, `go test -race ./...`, `go build ./...`,
  `scripts/check-plans`.
- Integration tests with the Pi fixture: resume via `/sessions` continues the
  earlier session id; a reply to an old answer switches and the next turn uses
  that session; search finds a phrase from a closed session and never one from
  the other principal; a restart during a turn keeps the session id.
- Live: discuss a topic, `/new`, ask about it ("what did we say about X?") and
  check that the model searches and answers; reply to the old answer and check
  that the context is back.

## Risks and open questions

- Pi's auto-compaction is on and replaces older history with a summary when a
  long session nears the context window. Search reads the raw file entries, so
  compacted history stays searchable. A compaction that starts near the end of a
  two-minute turn can still be cut off by the timeout; watch for it once
  sessions live longer.
- Session files are never deleted today. A retention period, for example 180
  days, is a separate decision.
- Starting a new session automatically after a long idle gap is deferred until
  resume and search have been used for a while.
- When several notices are pending, the paragraphs after the first carry no
  `[system]` marker, so such a turn's title or search text can include notice
  text. Notices are rare between turns; marking each paragraph is a separate
  change.
- Whether the provider accepts two user messages in a row after a resumed
  interruption is confirmed only by reading Pi's code; the live check should
  include a restart during a turn.
- If the application stops after Pi wrote a complete answer but before the turn
  was saved, the user sees the interrupted reply while the unseen answer stays in
  the resumed session, so the model may refer to it. The window is narrow.
- A search has per-file and per-line limits but no overall byte or time budget
  beyond the turn's tool deadline; at household volume the files stay small.
- A conversation reactivated by a reply can be older than the ten that
  `/sessions` lists, and then no row is marked current.
