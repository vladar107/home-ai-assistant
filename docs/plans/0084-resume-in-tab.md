# 0084 — Resume a conversation in its own tab

Status: `done`

## Context

Plan 0083 gave each Telegram tab its own conversation. Resume from
`/sessions` still works as before tabs: it makes the chosen conversation the
active one of the tab the list is in. That tab's current conversation is closed,
and the earlier messages are nowhere to be seen. A conversation open in another
tab only shows "open in another tab", with no way to get to it.

Bot API 9.4 lets a bot create a tab in a private chat (`createForumTopic`).
`forwardMessages` copies up to 100 messages at a time into a tab, in order, and
skips any the user deleted. The store keeps the Telegram message ID of each
request (`conversation_requests.telegram_message_id`) and of each text reply
(`conversation_reply_messages`) per session, and prunes neither.

## Goal

With tabs on, Resume in `/sessions` opens the conversation in a tab of its own:

- A conversation another tab holds stays there. The bot posts a short line in
  that tab, which moves the tab to the top of the list.
- Any other conversation gets a new tab named after it. Its earlier messages
  are forwarded into the new tab, and then the usual "Back in the conversation
  from …" line follows. The user's next message there continues it.
- The tab the list was opened in keeps its own conversation.

With tabs off, Resume works exactly as today.

## Decisions

- **Existing tab or a new one.** A conversation is never in two tabs. Pressing
  Resume on the conversation already in the current tab answers as today. In
  `/sessions`, a conversation another tab holds gets an Open button in place of
  Resume.
- **Forward the original messages.** They keep their exact text, photos, and
  files, and show as forwarded. Every message of the conversation is forwarded:
  the user's requests and the bot's replies, merged by message ID, in batches of
  100. Messages the user deleted are skipped by Telegram.
  - Cards, proposals, and "Done" messages are not recorded per session, so
    they are not forwarded. A continuation's request is the proposal it
    continued, so that proposal is forwarded.
  - The forwarded copies are not recorded as replies. A reply to one continues
    the tab's conversation, which is the same conversation anyway.
- **The tab name** is the conversation's title, cut to Telegram's 128
  characters. A conversation without a title is named "Conversation from
  <date>".
- **Failures.**
  - If the tab cannot be created, Resume falls back to today's behaviour in the
    current tab.
  - If the switch fails after the tab was made, the tab is deleted again.
  - If forwarding fails, the resume stands and the tab says the history could
    not be restored.
- **The list stays the record.** The `/sessions` message is refreshed after
  the press, as today, so the resumed conversation now shows as open in
  another tab.

## Approach

1. Telegram client: `CreateForumTopic`, `DeleteForumTopic`, and
   `ForwardMessages` (with `message_thread_id` from the context).
2. Store and conversation service: list a session's message IDs (requests and
   recorded replies, ascending, distinct). Return the tab a session is active
   in with the `/sessions` summaries.
3. Telegram service: with tabs on, the Resume press goes through the new flow:
   locate the conversation, then either nudge its tab, or create a tab,
   switch, forward the history in batches, and send the resumed line. With tabs
   off, the press keeps today's path.
4. README.

## Files affected

- `internal/telegram/` (client, sessions, service)
- `internal/conversation/` (service, types)
- `internal/store/sqlite/` (conversation history)
- `README.md`, `docs/plans/README.md`

## Validation

- `gofmt`, `go mod tidy` check, `go vet`, `go test -race ./...`, `go build`,
  and `scripts/check-plans`.
- Live check on the operator's bot with topics on: resume a closed
  conversation and an open one.

## Risks and open questions

- A long conversation forwards many messages at once. Telegram rate limits are
  met by one call per 100 messages, retried like any other send.
- Forwarded messages show a "Forwarded from" header. That is the price of
  keeping photos and files exact.
- Turning topics on in BotFather may not by itself let the bot create tabs if
  the operator blocks it in the Mini App setting. The fallback keeps Resume
  working.
