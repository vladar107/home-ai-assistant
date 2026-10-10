# 0063 — Show that a turn is still working

Status: `done`

## Context

A chat turn sends Telegram's typing action once, when it starts
(`conversation.Service.runTurn`), and once more while it waits behind another turn
(`awaitTurn`). Telegram clears typing after at most five seconds, so a turn that
runs for a minute looks idle for most of it, and the user cannot tell a slow turn
from a stuck one.

Bot API 9.3 added `sendMessageDraft`, an ephemeral preview at the bottom of a
private chat; since 9.5 every bot may use it. A draft lasts about 30 seconds,
disappears when the bot sends a message, and since 10.3 can carry a stop button
(`can_stop`) that sends the bot a `stopped_message_generation` update. These
versions and the update's fields are rechecked against the Bot API documentation
during implementation, and the version relied on is recorded here.

## Goal

While a chat turn runs, the user always sees that it is working and roughly what
it is doing, and can stop it from the chat:

- typing stays visible for the whole turn, including while it waits behind another
  turn;
- a turn that runs longer than a few seconds shows a draft such as
  "Searching the web… · 40s" that follows the turn's tool calls;
- the draft's stop button cancels the turn like `/cancel`.

## Decisions

- Only chat turns get the indicator. Scheduled runs and background tasks have
  nobody waiting on them, and tasks already send their own progress.
- Typing is resent every 4 seconds while a chat request waits for its turn slot and
  from the moment it is claimed until `runTurn` returns. A turn ends before its
  approval bundle or search confirmation is shown, so typing never runs while the
  bot waits on the user.
- The draft appears after 8 seconds, so quick replies never flash it, and is
  refreshed every 20 seconds. Its draft ID is derived from the request ID, so each
  turn has its own.
- The draft names the current activity, never tool arguments or results. A label
  belongs to a connector, not to each tool: tool contributions carry an activity key
  (for example `web`, `mail`, `calendar`, `files`, `home`, `transit`), and Pi's
  built-in web search maps to `web`. The broker hands the turn a name-to-key lookup
  beside the tool set; the key is never part of what the model sees. Before the first
  tool call, after a tool result, and for a tool without a key, the draft shows
  "Thinking…".
- Labels are English, like every other fixed reply the bot sends.
- Elapsed time is shown in whole seconds under a minute and whole minutes after.
- The stop button is honoured only from the principal's own private chat, authorized
  through the identity allowlist like any message, and only when its draft ID matches
  the turn running now; a late press after the turn ended changes nothing. A valid
  press cancels through `Service.Cancel` and gets the same confirmation `/cancel`
  sends, which also clears the draft.
- The keeper is best effort: an API error, including a 429, skips that tick and is
  counted; it is never retried and never fails or delays the turn. Logs carry the
  method and status, never draft text.
- The keeper stops and waits for its goroutine, cancelling any call in flight,
  before `runTurn` returns, so no typing action or draft lands after the reply.
  The sink hands it tool events without blocking, because `Emit` runs inside the
  runtime's event loop.

## Approach

1. Telegram client: add `SendMessageDraft(ctx, chatID, draftID, text, canStop)`,
   parse the `stopped_message_generation` update, and add it to the poller's
   `allowed_updates`. Dispatch it from `processUpdate` after private-chat and
   identity checks to the conversation service.
2. Conversation: a per-turn activity keeper with its own context, started when a
   connected turn is claimed. It owns the typing ticker, the draft timer, the current
   activity, and the turn's start time. Intervals and the clock come from `Config`,
   so tests run fast and deterministically. `awaitTurn` keeps typing alive the same
   way while it waits.
3. Tools: the broker exposes the activity key of each tool it offers, and the
   counting sink forwards tool calls and results to the keeper.
4. Telemetry: count typing and draft failures by method, with the series created at
   zero like the other conversation counters, and show them on the conversation
   dashboard in the monitoring repository.
5. Update `docs/architecture.md` where the chat turn's Telegram behaviour is
   described.

## Files affected

- `internal/telegram/client.go`, `internal/telegram/service.go`
- `internal/conversation/service.go`, `internal/conversation/types.go`, a new
  keeper file beside them
- `internal/tools/broker.go`, `internal/runtime/runtime.go` if the lookup travels
  with the tool set, and connector contributions (activity key)
- `internal/observability/telemetry.go`
- `docs/architecture.md`

## Validation

- Integration tests with the fake Telegram server, a fake runtime, and short
  configured intervals:
  - `sendChatAction` is sent at least once per interval through a long turn and
    while waiting behind another turn;
  - no draft before the delay, so a quick turn sends none;
  - the draft text follows tool calls and falls back to "Thinking…", and never
    contains tool arguments;
  - no `sendChatAction` or `sendMessageDraft` request after `runTurn` returns,
    whatever the outcome;
  - a matching stop update cancels the turn and sends the cancel confirmation; a
    stale, foreign, or non-private stop update changes nothing.
- Live check on the operator's GoLand server: how typing and a draft look together
  on a phone, the stop button, and that the draft disappears when the reply arrives.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- A failed turn's reply clears its draft, but a turn that ends with no message, such
  as one cancelled by `/cancel` from another device, leaves the draft until it
  expires, up to about 30 seconds.
- A busy reply or task status sent during the turn clears the draft until the next
  refresh.
- Telegram documents no rate limit for drafts; the 20-second cadence stays far below
  the general per-chat limit.
