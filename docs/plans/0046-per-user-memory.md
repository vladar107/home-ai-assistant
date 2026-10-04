# 0046 — Per-user memory the user can see and edit

Status: `in-progress`

## Context

Every conversation starts from nothing. The assistant forgets that a user prefers
short answers, which train they take, or what their children are called as soon
as they send `/new`, and scheduled runs never know any of it. Pi 1.0 has no
memory feature; it loads `AGENTS.md` context files from its agent directory and
every parent of the working directory, but the runtime passes
`--no-context-files`, and turning discovery back on would walk up from the
principal's workspace into whatever directory holds the data folder (in local
development, this repository, whose `AGENTS.md` holds the development rules).

Comparable assistants converge on the same shape: a small, curated memory that
is always in the prompt, written by the model through add/replace/remove
operations under a hard size cap, and visible to the user, who can delete any
entry. Their documented failure modes are memory bloat, stale facts, and
outside content (mail, web pages) persuading the model to store an instruction
that then applies to every later turn.

## Goal

Each user has their own memory. The assistant remembers what the user asks it to
and durable facts it learns in conversation, says so in the reply, and uses that
memory in every later chat turn, scheduled run, and background task of that user
only. `/memory` lists the entries and deletes any of them; memory can be turned
off in `/settings`.

## Decisions

- **Memory lives in SQLite, not in files.** A `memory_entries` table holds one
  row per entry: principal, kind, text, source, and timestamps. Rows give the
  list, delete, and cap operations for free and back up with the database. No
  `AGENTS.md` file is written, and `--no-context-files` stays.
- **Two kinds of entry.** *Instructions* say how the assistant should behave
  ("answer in English, briefly"). *Facts* are things about the user and the
  household ("prefers metric units"). Instructions change what every
  turn does, so the model can only propose one; it is written after Confirm
  through the existing approval bundle. Facts are written at once.
- **Hard caps force consolidation.** An entry is at most 300 characters.
  Instructions total at most 1,500 characters and facts at most 2,000. A write
  that would exceed a cap fails with a result that tells the model to merge or
  remove entries first. Exact duplicates are rejected.
- **The model writes through three tools.** `memory_add` (kind, text),
  `memory_replace` (id, text), and `memory_remove` (id) are write tools offered
  only in chat turns. Scheduled runs and background tasks read memory but cannot
  change it.
- **Outside content cannot write memory silently.** Once a turn of the
  conversation has read third-party content, every later fact write in that
  conversation becomes a proposal that needs Confirm, like an instruction. With
  safety checks on, the proposed text is also screened.
- **Memory goes into the system prompt, assembled by the application.** The
  runtime receives the rendered entries with their ids in `RunRequest` and adds
  them as a section after the tool guidance. The prompt changes only when memory
  changes, so it stays cacheable between writes; the per-turn time stays in the
  message as before.
- **Every write is visible in the reply.** A memory write stages a note for its
  turn the way cards are staged, and delivery appends a line such as
  `Remembered: prefers metric units` or `Forgot: …`. The user does not
  depend on the model mentioning it.
- **`/memory` is the deterministic view.** It lists instructions, then facts,
  each with a Delete button, paged like `/jobs`, with a "Forget everything"
  button behind a second confirmation. Turning memory off in `/settings` stops
  injection and removes the tools; entries are kept until deleted.
- `/privacy` gains a line saying what memory stores, where, and how to delete it.

### Refinements made while implementing

- The on/off switch is its own `memory_settings` table with a row only for a
  principal who changed it; memory is on by default. Entries carry an 8-character
  hex id that the prompt shows as `[id]` and the tools take; `source` is `model`
  for a direct write and `confirmed` for one written after Confirm.
- `memory_replace` of an instruction, or of any entry while the principal is
  exposed, is a proposal like `memory_add`. `memory_remove` is always immediate
  and shown in the reply as `Forgot: …`, since deleting is what the user can do
  in `/memory` anyway.
- Exposure is a flag on the conversation session (`exposed_at`), not the
  30-minute per-principal window in `turnstate`. Third-party text stays in the
  Pi session until `/new`, so a window would let a write slip through after 30
  minutes or after a restart. A per-principal mark would also let a scheduled run
  that read mail make an unrelated conversation's writes need Confirm. The
  broker marks the session of a chat turn whose third-party tool succeeded. The
  conversation marks it when Pi's own `web_search` runs, which never passes
  through the broker. A write outside a chat turn, or one whose lookup fails,
  counts as exposed. `turnstate` is unchanged and still exists only with TypeSafe.
- A confirmed proposal is not written while memory is off; the user is told so.
- With safety checks on, only a
  fact proposed because of exposure is screened: an instruction always
  addresses the assistant, so screening it says nothing. Flagged text is still
  proposed, with a warning line above it, because the user decides either way.
- The reply notes (`Remembered:`, `Updated:`, `Forgot:`) are appended to the
  reply text when the turn finishes, before the response is saved, so they
  survive a restart with the reply. They are staged in memory keyed by turn.
- Caps and duplicates are checked when the proposal is made and again when the
  confirmed proposal executes. The model sees the real reason; the user sees a
  short one.
- `/memory` callbacks use the prefix `mem`, because `memory` is the settings
  section's prefix. `/memory` is added to the registered command list when the
  memory service is present; `HelpReply` is unchanged, since it does not list
  every command today.
- The `/privacy` line is shown whenever memory exists, not only while it is on,
  because entries stay stored while it is off.
- Telemetry is the counter `home_agent.memory.writes` with kind and outcome
  (`written`, `proposed`, `removed`, `rejected_cap`, `rejected_duplicate`),
  without content.
- The Pi runtime appends `RunRequest.Memory` to the prompt it already builds,
  after the tool guidance. Chat and scheduled turns render memory at turn start;
  a failure to read it fails the turn rather than running it without memory.
  Scheduled runs read memory from the prompt and are offered no memory tools.
  Wiring memory into background tasks is left to plan 0048.
- Because memory is on by default, every chat turn now carries the memory tools.
  A principal with no integrations therefore gets the tool prompt and a bridge
  instead of the no-tools prompt, and tool-call metrics and the TypeSafe tool
  hint now see every turn. The no-tools prompt remains for turns with memory off.
- A turn cancelled by `/cancel` or `/new` sends no reply, so the note for a fact
  it already wrote is not shown. The entry is visible in `/memory`.

## Approach

1. Migration `014_memory.sql`: `memory_entries` with a kind check, length
   check, and a per-principal index; the per-principal on/off switch beside the
   other `/settings` switches. Store methods for list, add, replace, remove, and
   clear, each scoped by principal. Complexity: simple.
2. `internal/memory`: the service with caps, duplicate detection, rendering for
   the prompt, and the tool provider for the broker. Writes that need Confirm go
   through `approval.Propose`; the exposure check reads the conversation
   session's flag. Complexity: moderate.
3. Runtime: `RunRequest.Memory`, rendered by `systemPrompt` in the Pi runtime;
   conversation, scheduled, and (later) task turns load it at turn start.
   Complexity: simple.
4. Telegram: `/memory` with paging and Delete callbacks, the `/settings` switch,
   the reply note, `/help` and `/privacy` text, and the command registration.
   Complexity: moderate.
5. Telemetry: memory writes by kind and outcome (written, proposed, rejected by
   cap), without content.
6. Update `README.md`, `docs/architecture.md`, and `.env.example` if a feature
   flag is added.

## Files affected

- `internal/store/sqlite/migrations/014_memory.sql`, `internal/store/sqlite/memory.go`
- `internal/memory/` (new)
- `internal/runtime/runtime.go`, `internal/runtime/pi/runtime.go`
- `internal/conversation/service.go`, `internal/conversation/scheduled.go`
- `internal/telegram/` (`/memory`, settings, reply note, commands)
- `internal/app/app.go`
- `README.md`, `docs/architecture.md`

## Validation

- `gofmt`, `go vet ./...`, `go test -race ./...`, `go build ./...`,
  `scripts/check-plans`.
- Integration tests through the Telegram harness: a fact written in one session
  is in the prompt of the next one after `/new`; another principal never sees
  it; a write after a third-party tool result becomes a proposal; a cap overflow
  returns the consolidation result; `/memory` Delete removes the entry from the
  next prompt; the off switch removes both the section and the tools.
- Live: tell the assistant a preference, `/new`, and check that the next answer
  uses it; delete it from `/memory` and check that it no longer does.

## Risks and open questions

- The model may store too little or too much. The guidance says what is worth
  remembering (stable preferences and facts the user would expect it to know
  next week), not chat details. Tune it after a week of real use.
- Facts go stale. There is no expiry; the model is told to replace an entry that
  the conversation contradicts. Timestamps make an age-based review possible
  later.
- A save before Pi compacts a session (Pi's `session_before_compact` hook) or
  before `/new` would catch facts the model never stored. It costs an extra
  model call per session and is deferred.
