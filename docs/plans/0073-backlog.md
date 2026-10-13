# 0073 — A backlog of ideas for the assistant

Status: `done`

## Context

While talking to the bot, the user often thinks of something the assistant
should do differently or learn to do, and wants to implement it later in a
development session. Today there is nowhere to put it: memory is for facts and
instructions the assistant should apply, and a reminder fires at a time. The
idea gets lost, or ends up in memory where it changes behaviour.

## Goal

- The user can say "add to the backlog: …", "idea for later: …", or "remember
  to implement …", and the bot keeps it as a backlog item, with the context
  that explains it.
- `/backlog` shows open items, lets the user mark them done or delete them, and
  can send the whole list as one message to paste into a development session.
- Backlog items never influence how the assistant behaves.

## Decisions

- **Its own store, per principal.** A `backlog_items` table: ID, principal,
  title (1–120 runes), details (0–2000 runes), status (`open`, `done`),
  created and updated times. It cascades with the principal. Each principal
  may keep 200 open items.
- **Tools.** `backlog_add` (title and details), `backlog_list` (open items, or
  done ones on request), and `backlog_update` (by ID: new title or details, or
  status). Details should hold what the user said and why, written so that a
  developer without the conversation can act on it. `backlog_list` shows the
  newest 20 items with details shortened to 150 characters, so a long backlog
  stays inside the broker's result limit; given an ID, it returns that item in
  full.
- **Chat turns only.** All three tools, the read included, are offered on chat
  turns alone: the backlog is the user's own list, and a scheduled run or
  background task has no reason to read or write it.
- **No confirmation for the user's own notes.** Adding or updating an item
  changes nothing the assistant does and nothing outside the principal's own
  list, so it runs without a Confirm step. The reply notes what was added
  through the model: the tool result names the item and the guidance asks for
  one short line, which is simpler than a second notes channel like memory's.
- **Confirmation after outside content.** The broker has no general gate for
  writes after outside content; each provider applies it, as reminders, tasks,
  and home actions do. The backlog follows them: once the conversation has
  read text from outside the household, `backlog_add` and `backlog_update`
  become proposals the user confirms, because an exported item is pasted into a
  development session and must not carry instructions from a web page or mail.
- **Kept out of the prompt.** Unlike memory, backlog items are never put in
  the system prompt or turn message. The model reads them only through
  `backlog_list`.
- **Guidance.** Use the backlog when the user describes something the
  assistant itself should do or do differently later, or asks to note an idea
  to build; use memory for facts and preferences the assistant should apply
  now; use a reminder for something the user must do at a time.
- **`/backlog` command.** Lists open items with their title and age, each with
  "Done" and "Delete" buttons, like `/memory`. `/backlog export` sends all open
  items as one Markdown message (split if long): title, date, and details per
  item.
- **Help.** `/help` lists `/backlog`, and the registered command list, which
  `app_help` reads for its commands topic, includes it.

## Approach

1. Store: migration `024_backlog.sql` and a repository.
2. A new `internal/backlog` package: the service and the tool provider,
   modelled on `internal/memory`.
3. Telegram: `/backlog` and `/backlog export`, and its callbacks, modelled on
   `/memory`; the command in help.
4. Wiring in `internal/app`.
5. README, `docs/architecture.md`.

## Files affected

- `internal/store/sqlite/migrations/024_backlog.sql`, a new
  `internal/store/sqlite/backlog.go`
- a new `internal/backlog` package
- a new `internal/telegram/backlog.go`, `internal/telegram/service.go` (the
  command, its `export` argument, the callbacks, and the help line)
- `internal/app/app.go`
- README, `docs/architecture.md`

## Validation

- Integration tests with a temporary SQLite database and the fake Telegram
  server:
  - `backlog_add` stores an item and the reply notes it; `backlog_list` and
    `backlog_update` work by ID;
  - another principal's items are never listed or changed;
  - `/backlog` lists open items; Done and Delete change them;
  - `/backlog export` sends the items' details;
  - scheduled turns are not offered the tools;
  - backlog items never appear in the system prompt.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- The model may put real reminders or preferences in the backlog, or ideas in
  memory; the guidance draws the line, and `/backlog` makes mistakes visible.
- Picking items up is manual: the user exports them and pastes them into a
  development session.
