# 0083 — Chat tabs

Status: `done`

## Context

Bot API 9.3 lets a bot turn on topics in private chats through BotFather.
Telegram clients show them as tabs, and each message then carries a
`message_thread_id`. `getMe` reports the switch as `has_topics_enabled`.

The assistant ignores the thread today. Everything a chat turn owns is keyed
by principal only:

- The store allows one active conversation per principal
  (`conversation_sessions_one_active_principal_idx`) and one pending request
  per principal (checked in `EnqueueConversation` and `SwitchConversation`).
- `conversation.Service` runs one worker per principal and keeps `inflight`,
  `drafts`, and the turn slot by principal. `/cancel`, `/new`, `/sessions`,
  and the draft stop button act on that one conversation.
- Approval outcome notices and deferred continuations are kept by principal.
  The `turnstate` exchange that guards read holds the principal's last
  messages and reply.
- Sends name a chat ID only. With topics on, a send without a thread lands in
  the main view, so every reply would leave the tab it answers.

The slash menu lists ten commands in the order the code happens to register
them.

## Goal

- With topics on, each tab is its own conversation with its own session. Tabs
  run their turns in parallel. A reply, its draft, typing, cards, proposals,
  and task results stay in the tab of the message they answer.
- `/new`, `/cancel`, and `/sessions` act on the tab they are sent in.
  `/sessions` can bring an earlier conversation into the current tab.
- Reminders, scheduled job reports, watches, trip messages, and other
  messages nobody asked for in a tab go to the main view.
- With topics off, nothing changes.
- The slash menu is shorter and sorted alphabetically.

## Decisions

- **The switch is BotFather's.** The service reads `has_topics_enabled` from
  `getMe` at start-up. No new setting. When it is off, every thread ID is
  treated as 0, so behaviour stays exactly as it is today.
- **A lane is a principal and a thread.** Thread 0 is the main view, and it
  is the only lane when tabs are off. Sessions and requests store their
  thread. The active-session index and the pending-request check become
  per lane.
- **Tabs run in parallel.** The worker, `inflight`, draft, and turn slot are
  kept per lane. Scheduled runs take the main lane's slot, as they do today.
  The original reason for one slot per principal was a credential-refresh
  race, and Pi 1.0.0 fixed it by locking `auth.json`. Background tasks
  already run beside chat turns.
- **The thread travels with the context.** A thread set on the context makes
  the Telegram client add `message_thread_id` to sends, drafts, and chat
  actions. Edits, deletions, and reactions name a message, so they don't need
  it. A lane's worker sets its thread, so everything a turn sends lands in the
  right tab. Proactive senders start from contexts that carry no thread, so
  their messages land in the main view without any change to them. Persisted
  work that is delivered later restores the thread from its row (approvals,
  tasks).
- **A session belongs to one lane.** A reply to an answer from another
  conversation in the same tab returns to it, as today. `/sessions` in a tab
  can resume a closed conversation into that tab. One that is active in
  another tab is marked "open in another tab" and has no Resume button.
- **A deleted tab falls back to the main view.** When Telegram says the
  thread no longer exists, a delivery is sent once more without the thread.
- **Per-lane memory where a turn could mix tabs.** Approval notices and
  deferred continuations are kept by lane, so a Confirm continues in the tab
  of the proposal. The `turnstate` messages and replies that guards compare
  against are kept by lane. Exposure and suspicion marks stay per principal,
  which is the conservative choice. Watch, task, and job-report notices are
  still taken by the next turn of any tab.
- **Slash menu.** It lists `backlog`, `cancel`, `jobs`, `memory`, `new`,
  `sessions`, `settings`, and `tasks`, alphabetically, each shown only when its
  feature is on. `/status` and `/privacy` leave the menu but still work, and
  the help text and `/settings` name them.

## Approach

1. Telegram transport: read `has_topics_enabled`, `Message.MessageThreadID`,
   and `is_topic_message`. Add the context thread and send `message_thread_id`
   from every send method. Recognise the "thread not found" rejection.
2. Store: migration 031 adds `thread_id` to `conversation_sessions`,
   `conversation_requests`, `approvals`, and `tasks`. It replaces the
   active-session index with a per-lane one. The enqueue, switch, reset,
   cancel, recover, and next-request queries become per lane.
3. Conversation service: key workers, `inflight`, drafts, and turn slots by
   lane. Thread the lane through `Enqueue`, `Cancel`, `NewSession`, `Resume`,
   `Sessions`, and `StopDraft`. Set the thread on the worker context. Fall back
   to the main view on a deleted thread.
4. Approval, task, reaction, and turnstate: store and restore the thread, and
   key notices, continuations, and exchanges by lane.
5. Telegram service: take the thread from messages, callbacks, and stop
   presses when tabs are on. Commands and callbacks reply in their tab. A
   reaction's reconsider goes to the lane of the reply's session.
6. Slash menu and help text.
7. Docs: README and the operator notes on turning topics on in BotFather.

## Files affected

- `internal/telegram/` (client, service, commands, sessions, help, settings,
  tasks)
- `internal/conversation/` (service, types, activity, reaction, scheduled)
- `internal/store/sqlite/` (conversation, conversation history, approvals,
  tasks, new migration)
- `internal/approval/`, `internal/task/`, `internal/turnstate/`,
  `internal/judgment/`
- `README.md`, `docs/plans/README.md`

## Validation

- `gofmt`, `go mod tidy` check, `go vet`, `go test ./...`, `go build`, and
  `scripts/check-plans`.
- Integration tests for: two tabs holding separate sessions and running at the
  same time, `/new` and `/sessions` scoped to a tab, a reminder landing in the
  main view while a tab is in use, and topics off sending no thread.
- Live check on the operator's bot after turning topics on in BotFather.

## Risks and open questions

- A May 2026 report says sends to private-chat topics sometimes fail with
  "message thread not found" after Bot API 10.0. The fallback to the main view
  keeps such replies from being lost.
- Parallel tabs double the load on one subscription when used together. Usage
  limits surface as the existing quota reply.
- Naming tabs after their first message (`editForumTopic`) is left for later.
