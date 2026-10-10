# 0064 — Reactions both ways

Status: `done`

## Context

Every chat turn ends in a text reply, even when the user only said "thanks" or
"ok", so the bot answers small acknowledgements with filler. The user also has no
light way to tell the bot that a reply was wrong other than writing it out.

Telegram lets a bot set one emoji reaction per message from a fixed set
(`setMessageReaction`), and delivers `message_reaction` updates when a user reacts,
if the bot asks for them in `allowed_updates`. Telegram documents those updates for
groups, where the bot must be an administrator; whether private chats deliver them
is not documented. The update carries the message ID and the old and new reaction
lists, but no message text.

## Goal

- The model can react to the user's message: a reaction alone for an
  acknowledgement, or a reaction together with a reply when that fits better.
- The user can react to the bot's replies: reactions are recorded as feedback, and
  👎 makes the bot look at that reply again.

## Decisions

### The bot reacts

- A `react` tool, contributed by a small core provider. Its schema allows one emoji
  from a curated subset of Telegram's bot reactions (👍 👌 ❤ 🔥 🎉 🤝 🫡 🤔 😁 🙏).
  The call applies the reaction to the user's message at once; a later call in the
  same turn replaces it. It stays inside the turn's tool budget.
- It is offered only on turns that answer a message the user wrote. `runTurn` marks
  such turns on the turn's run record; continuation turns after Confirm and
  reconsider turns are not marked, because their Telegram message is one of the
  bot's own. The provider reads the turn's chat and message through the existing
  task-origin lookup.
- Guidance: react rarely. Prefer a reaction alone when the message needs no answer,
  such as thanks or an acknowledgement; add a reaction to a reply only when it adds
  warmth; never react to a request instead of doing it. When the reaction alone is
  the answer, reply with exactly `NothingToAdd`. Delivery already sends nothing for
  it, keeps no reply, and still runs the follow-up. An empty response stays a runtime
  failure.
- When delivery strips `NothingToAdd` and nothing is left, it sends nothing at all,
  including the line that marks a resumed conversation.
- The tool needs no confirmation or intent check: it touches only the user's own
  message in their own chat, and no gate applies to it today.

### The user reacts

- A reaction is honoured only from an allowlisted principal in their private chat,
  on a message recorded as one of that principal's delivered replies. An update
  without a user, such as one from a channel identity, is ignored, as is any other
  message. Reactions on card photos are ignored, since photos are not recorded.
- The new reaction list decides the class: negative if it contains 👎, positive if it
  contains 👍 ❤ 🔥 👌 🎉 or 🙏, otherwise other; an empty list removes the feedback.
  Only the latest state per message is kept, and no message text is stored.
- The first 👎 on a reply starts one reconsider turn in the reply's conversation. The
  once-only flag belongs to the reply's request, so a reply delivered in several
  chunks starts at most one, whatever reactions follow. Replies recorded before this
  change have no request; their reconsider is keyed by the message instead.
- The reconsider message is ours and fixed: it says the user marked one of the
  model's replies as wrong, names when that reply was sent so the model can find it
  in its session, and asks it to look again. It quotes no reply text, because none is
  stored and the turn's message is trusted as the user's instruction. The turn quotes
  the 👎'd message in Telegram.
- Turns whose message is ours are recognised by one helper, used where the code now
  compares with `ContinueMessage`, so neither is recorded as the user's request for
  safety judgments or used for a tool hint.
- A 👎 on a reply from another conversation switches to it, closing the current one,
  the same way replying to an old message does.
- When the principal already has a turn pending, the reconsider is not started: the
  feedback is recorded without the flag, so a later 👎 can still start one, and
  nothing is sent.
- The reconsider is enqueued before its flag is set, and enqueuing is idempotent per
  Telegram update, so a redelivered update cannot start two.
- The bot's own reactions produce no updates, so there is no echo loop.

## Approach

1. Telegram client: `SetMessageReaction(ctx, chatID, messageID, emoji)`, with an
   empty emoji clearing it; parse `message_reaction` and add it to
   `allowed_updates`; dispatch it from
   `processUpdate`, logging only its arrival at first.
2. Live check on the operator's GoLand server that reactions in a private chat
   arrive. If they do not, keep the bot-side half and mark the user-side half
   abandoned in this plan.
3. React tool: the provider, the user-message mark on the run record, guidance, and
   the delivery fix for a resumed turn that sends nothing.
4. Store: migration `021` adds the request ID to `conversation_reply_messages`
   (empty for older rows) and a `reply_feedback` table keyed by principal and
   Telegram message, holding the class, the emoji, and the time, plus a reconsider
   record keyed by request, or by message for older replies. Both cascade with the
   reply records, sessions, and principals, so `/new` and removing a principal clean
   them up. Store methods to upsert and delete feedback and to claim a reconsider.
5. Reaction handling: authorize, record feedback, count it by class in telemetry with
   the series created at zero, and on the first 👎 enqueue the reconsider turn.
   Count the bot's own reactions and reconsider turns too, and show all three on the
   conversation dashboard in the monitoring repository.
6. Update `docs/architecture.md` for both directions.

## Files affected

- `internal/telegram/client.go`, `internal/telegram/service.go`
- A new reaction provider package under `internal/`, and `internal/app/app.go`
- `internal/conversation/service.go`, `internal/conversation/types.go`
- `internal/tools/run.go`
- `internal/store/sqlite/` with migration `021`
- `internal/observability/telemetry.go`
- `docs/architecture.md`

## Validation

- Store tests with a temporary SQLite database: the reply-request link, feedback
  upsert, change, and removal, the reconsider claim, and cascading cleanup.
- Integration tests:
  - a react call reaching the fake Telegram server;
  - a react-only turn sending no text and still running the follow-up, also on a
    resumed conversation;
  - a reaction with a reply sending both;
  - continuation and reconsider turns not offering `react`;
  - a reaction update producing a feedback row, and removal deleting it;
  - a first 👎 enqueuing exactly one reconsider turn, also for 👎 on two chunks of one
    reply and for a redelivered update;
  - a 👎 while a turn is pending recording feedback without starting a reconsider;
  - foreign, non-private, user-less, and unknown-message reactions doing nothing;
  - a reconsider message not recorded as the user's message and starting no tool
    hint.
- Live check of reaction delivery in a private chat, then of the full flow.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- If private chats never deliver `message_reaction`, the user-side half is dropped.
- The model may over-react; guidance and a look at how often `react` is called
  will tell.
- A silent acknowledgement turn that runs past the draft delay sends nothing that
  clears its draft, so the draft stays until it expires.
- A 👎 pressed while a turn is pending does nothing visible; if that confuses, the bot
  could later answer it once the turn ends.
