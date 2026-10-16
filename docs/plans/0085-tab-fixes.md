# 0085 — Tabs that tell the truth, and closing a tab

Status: `in-progress`

## Context

Three backlog items report that `/sessions` misleads in tabs:

- `548b737c`: the Open buttons answer "That conversation is open in its own
  tab, which is now at the top of your tabs" when that is not true. Some
  entries show "open in another tab" and others offer Resume, and the user
  cannot tell why.
- `12fffd1b`: Open says the conversation is open but does not take the user
  there.
- `748b6c58`: a tab made by Resume cannot be configured or deleted.

The causes in the code:

- A tab's conversation stays `active` with its `thread_id` until the user
  sends `/new` in that tab or resumes something else there.
- Telegram does not tell the bot when a user deletes a tab.
- A bot-created tab can also be broken from the start: `createForumTopic`
  succeeds, but every write then fails with "message thread not found"
  ([tdlib/telegram-bot-api#905](https://github.com/tdlib/telegram-bot-api/issues/905)).
  Either way, the conversation is listed as "open in another tab" for good.
- On Open, `resumeInTab` posts `SessionHere` into that tab. When the tab is
  gone, `Client.sendMessage` silently resends without the thread, so the line
  lands in the main view. The press still answers `SessionInItsTab`. Nothing
  is logged.
- `sendPlain` (trip messages) never sends a thread. `SendPhoto` and
  `SendDocument` send one, but they have no fallback, so a photo sent to a
  gone tab fails every time.
- Every tab the user has written in holds an active conversation, so many
  entries are rightly "open in another tab". The list does not say which tab.
- The Bot API has no method that switches the user's client to a tab, and no
  documented link to a tab in a private chat. Posting in the tab, which moves
  it to the top of the list, is as far as a bot can go.
- Whether a user may delete a tab the bot made depends on Telegram and the
  BotFather setting behind `allows_users_to_create_topics`. The bot can always
  try `deleteForumTopic` itself.

## Goal

- A conversation whose tab is gone is no longer shown as open elsewhere. It
  can be resumed into a new tab, and it comes back by itself if the user writes
  in that tab again.
- `/sessions` says which tab holds each open conversation.
- Open names the tab it points to, says the bot cannot switch tabs, and never
  claims a tab that is gone.
- The user can close a tab from inside it, or from `/sessions`. Closing ends
  that tab's conversation, keeps it in `/sessions`, and deletes the tab.
- With topics off, nothing changes.

## Decisions

- **Migration 032 rebuilds `conversation_sessions`, once.**
  - SQLite cannot widen the `close_reason` check in place, so the table is
    rebuilt the way migration 015 did it. The rebuild keeps everything that
    references the table through `(session_id, principal_id)`:
    `conversation_requests`, `conversation_reply_messages`, and attachments.
  - It carries over migration 031's `thread_id`, and recreates
    `conversation_sessions_one_active_lane_idx`.
  - It adds `tab_gone` and `tab_closed` to the close reasons.
  - It also adds the nullable `subject` column that plan 0087 needs. Adding it
    here means the table is rebuilt only once.
- **A tab registry.**
  - Migration 032 also adds `telegram_tabs`, which is STRICT, keyed by
    principal and thread, and cascades with the principal. Its columns are:
    - principal ID and thread ID
    - name
    - `named_by`: `bot`, `user`, or `fixed`. `fixed` is for feed and trip
      tabs (plans 0087 and 0089), which are never renamed by topic.
    - `confirmed_at`: the first time a send into the tab, or a user message
      from it, succeeded
    - created and updated times
  - A row is written when the bot creates a tab.
  - **Topic service messages are this plan's.** The update parser reads
    `forum_topic_created` (with `is_name_implicit`) and `forum_topic_edited`,
    both on a message and on its `reply_to_message`.
    - A created tab is recorded with its name. It is `named_by = bot` when
      its name was implicit, and `user` otherwise.
    - An edit sent by the user sets the name and `named_by = user`. An edit
      that comes from the bot itself, the echo of `editForumTopic`, changes
      nothing.
    - Each one is logged as `telegram.topic.service`, with the thread ID and
      which fields were present, never the name. This log is how plan 0088
      learns what Telegram sends in private chats.
  - The registry holds names. Sessions stay the record of which conversation
    a tab holds.
- **Every send that names a tab reports a gone tab.**
  - One wrapper in the client covers every method that sends a thread:
    `sendMessage`, `sendPlain`, `SendPhoto`, `SendDocument`, and the keyboard
    sends.
  - On "message thread not found", the wrapper resends once without the
    thread, so the message reaches the main view, and it reports the thread
    to an observer. It logs `telegram.send.thread_gone` with the principal
    and thread IDs.
  - `sendPlain` starts sending the context's thread.
  - `ForwardMessages` and `deleteForumTopic` report the thread but do not
    fall back: a forward goes only into the tab it was meant for.
- **Gone or suspect.** Some "thread not found" answers are Telegram's bug and
  not a real deletion. The observer treats the report as suspect, and keeps
  everything, when either of these holds:
  - The user wrote in that thread in the last 24 hours.
  - The tab was created by the bot, has never been confirmed, and is less
    than 24 hours old.

  A suspect report logs `telegram.send.thread_gone_suspect` and changes
  nothing. Otherwise the tab is treated as gone:
  - The lane's active session closes with `tab_gone`.
  - The registry row is dropped.
- **A gone tab can come back.** When a message arrives in a tab that has no
  active conversation, the store first reopens the most recent session closed
  as `tab_gone` in that tab, if there is one, and only then would create a
  new one. `tab_closed` is never reopened this way.
- **`/sessions` names tabs.** An entry open elsewhere reads "open in “Trip
  notes”" when the registry knows the name, and "open in another tab" when it
  does not.
- **Honest Open.**
  - The reply names the tab: "That conversation is in the tab “Trip notes”.
    Telegram does not let me switch tabs for you, so I posted there to bring
    it to the top."
  - If the nudge in that tab is reported gone, the lane is closed as
    `tab_gone`, and the press continues as a resume into a new tab with the
    history forwarded, as plan 0084 does.
  - A suspect report answers that the tab did not accept the message, and
    offers *Close N*.
- **Closing a tab.**
  - `/close` in a tab answers with a confirmation: "Close this tab? Telegram
    deletes its messages, so resuming the conversation later cannot bring them
    back. The conversation stays in /sessions." The buttons are *Close tab*
    and *Keep*.
  - Confirming does three things, in order:
    1. It cancels the lane's queued or running turn, so no reply lands in
       the main view.
    2. It closes the lane's session with `tab_closed`.
    3. It calls `deleteForumTopic` and drops the registry row.
  - If the delete fails, the session stays closed. The reply, sent in the
    main view, says the tab could not be removed and can be deleted from its
    menu.
  - `/close` in the main view answers that the main view cannot be closed and
    that `/new` starts a fresh conversation.
  - In `/sessions`, an entry open in another tab gets a *Close N* button
    beside *Open N*, and it confirms the same way. This is how a broken tab
    the user cannot enter gets cleaned up.
  - `/close` joins the slash menu in alphabetical order, listed only when
    topics are on.
- **Telemetry** counts `tab_gone`, `tab_gone_suspect`, and `tab_closed` with
  fixed outcome labels. Logs carry IDs only, never tab names.

## Approach

### Chunks

1. **Store and conversation service.** Jev: complex (1.00).
   - Migration 032 with the rebuild and `telegram_tabs`.
   - Registry upsert, read, confirm, and delete.
   - A per-lane close with a reason.
   - The reopen-on-message step in `EnqueueConversation`.
   - The lane's last user message time, for the suspect rule.
   - `conversation.Service.CloseLane(ctx, lane, reason)`, which cancels the
     lane's turn first.
   - Tab names on `SessionSummary`.
   - Acceptance, from integration tests on real temporary SQLite:
     - The migration keeps existing sessions, requests, reply messages, and
       attachments, and the per-lane active index still refuses two active
       sessions in one lane.
     - A lane closed as `tab_gone` is not listed as open elsewhere, can be
       resumed, and is continued by the next message in that thread.
     - `tab_closed` is never reopened by itself.
     - `CloseLane` cancels a queued turn.
2. **Telegram.** Jev: complex (0.95). Depends on chunk 1.
   - The send wrapper and its observer, with the gone and suspect rules.
   - The topic service messages.
   - Tab names in `/sessions`, the Open wording and its gone-tab path, and
     `/close` with *Close N*.
   - The slash menu, the help text, and the README.
   - Acceptance, from integration tests against a fake Telegram boundary:
     - Each wrapped send method falls back once and reports the thread.
     - A tab the user wrote in an hour ago is reported suspect, and the same
       tab two days later is reported gone.
     - Open on a live tab names it and posts there.
     - Open on a gone tab creates a new tab and forwards the history.
     - `/close` cancels a running turn, closes the session, and deletes the
       tab. A failed delete still closes the session and says so.
     - `/close` in the main view refuses.
     - An edit echoed from the bot does not set `named_by = user`.
     - With topics off, no tab calls are made.

### Waves

- Wave 1: chunk 1.
- Wave 2: chunk 2.

Plans 0085 to 0089 land in that order, so their migrations are 032 to 036.

## Files affected

- `internal/store/sqlite/`: `migrations/032_tabs.sql`, `conversation.go`,
  `conversation_history.go`, and the new `tabs.go`
- `internal/conversation/`: `service.go`, `types.go`
- `internal/telegram/`: `client.go`, `trips.go` (`sendPlain`), `sessions.go`,
  `service.go`, `commands.go`, `help.go`
- `internal/observability/telemetry.go`
- `internal/app/app.go` (the observer wiring)
- `README.md`, `docs/plans/README.md`

## Validation

- `gofmt`, a `go mod tidy` diff, `go vet`, `go test -race ./...`,
  `go build`, and `scripts/check-plans`.
- A live check on the operator's bot with topics on:
  - Delete a tab in Telegram, then press Open on its conversation in
    `/sessions`.
  - Close a tab with `/close`.
  - Create and rename a tab, and read the `telegram.topic.service` logs that
    plan 0088 needs.

## Risks and open questions

- Telegram may never answer "thread not found" for some deleted tabs, for
  example when nothing is sent there again. Such a conversation stays listed
  until the user presses Open or Close on it, which then finds out.
- The suspect rule can keep a really deleted tab for up to a day when the
  user wrote in it just before deleting it. Its messages still reach the main
  view in that time.
- If Telegram does not send topic service messages in private chats, user
  tabs have no name in the registry, and the list and Open fall back to
  "another tab".
- `deleteForumTopic` may fail on tabs hit by the Bot API bug
  (`TOPIC_ID_INVALID`). The session is still closed, so the entry leaves the
  open list.
- The rebuild in migration 032 cannot be undone. It follows the tested
  pattern of migration 015.
