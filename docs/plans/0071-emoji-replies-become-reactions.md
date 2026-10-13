# 0071 — Emoji-only replies become reactions

Status: `done`

## Context

Plan 0064 gave the model a `react` tool and told it to answer an
acknowledgement with a reaction and the reply `[nothing-to-add]`. In use, the
model sometimes skips the tool and replies with the emoji as text, so the user
gets a message such as "👍" instead of a reaction on their own message. It also
happens that the model reacts and then still sends the same emoji as text.

Whether the model calls the tool is a judgment that guidance alone does not
make reliable. Whether a finished reply is only an emoji is something code can
see.

## Goal

- A chat turn that answers a message the user wrote, and whose whole reply is
  one or a few emoji, ends as a reaction on the user's message and sends no
  text.
- A turn that already set a reaction and then replies with only an emoji sends
  no text.
- Replies with words in them are never touched.

## Decisions

- **Code converts the reply after the run.** After a successful chat turn,
  when the run is marked as answering a user message and the trimmed reply
  consists only of emoji (with variation selectors, skin tones, and joiners,
  and whitespace between them), the turn treats it as a reaction:
  - If the run already set a reaction, the reply becomes `NothingToAdd`.
  - Otherwise, the first emoji is normalised (variation selectors and skin
    tones removed, since Telegram's reactions take neither) and, when it is in the `react` tool's curated set, set as the reaction on
    the user's message through the same path the tool uses. The reply becomes
    `NothingToAdd`.
  - An emoji outside the curated set, or a failed reaction call, leaves the
    reply as it was, so the user still gets an answer.
- **The run remembers a reaction.** The run record gains a mark that the
  `react` tool sets on success, so the conversation service can tell whether
  this turn reacted.
- **The conversion lives behind a small interface.** The conversation service
  gets an optional `Reactions` dependency, implemented by the reaction
  provider, so `internal/conversation` does not import `internal/reaction`.
  Without it, replies are delivered unchanged. The conversation service only
  detects an emoji-only reply and passes its first emoji; the provider, which
  reads the run from the turn's context, checks the user-message and reacted
  marks and the curated set, so those rules stay beside the tool's.
- **Guidance is sharpened, not relied on.** The `react` guidance adds: never
  answer with only an emoji as text; use `react` for that.
- **Telemetry.** The reaction is counted like any other bot reaction, and the
  conversion is counted separately in `home_agent.reactions.converted` by
  outcome (`reacted` when the emoji became the reaction, `dropped` when the
  turn had already reacted), so the dashboard shows how often the model still
  writes emoji as text.

## Approach

1. `internal/tools/run.go`: a reacted mark on the run.
2. `internal/reaction`: mark the run on success; expose a method that sets a
   reaction for a chat turn's request from a reply's emoji; sharpen guidance.
3. `internal/conversation`: detect an emoji-only reply on a user-message chat
   turn and convert it through the new interface before the response is saved.
4. `internal/app`: wire the reaction provider into the conversation config.
5. `internal/observability`: count conversions, with the series created at
   zero.
6. `docs/architecture.md`: one line in the reactions section.

## Files affected

- `internal/tools/run.go`
- `internal/reaction/reaction.go`
- `internal/conversation/service.go`, `internal/conversation/types.go`,
  `internal/conversation/reaction.go`
- `internal/app/app.go`
- `internal/observability/telemetry.go`
- `docs/architecture.md`
- `internal/reaction/reaction_test.go`,
  `internal/telegram/conversation_integration_test.go`,
  `internal/telegram/reaction_integration_test.go`

## Validation

- Integration tests through the fake Telegram server and fake runtime:
  - a reply of only "👍" with no tool call sets the reaction and sends no text;
  - a turn that called `react` and replies with only an emoji sends no text;
  - an emoji outside the curated set is sent as text;
  - a reply with words and an emoji is sent unchanged;
  - a continuation or reconsider turn replying with an emoji is not converted.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- A user who asks "send me a thumbs up" gets a reaction rather than a message;
  that is acceptable for a private chat.
