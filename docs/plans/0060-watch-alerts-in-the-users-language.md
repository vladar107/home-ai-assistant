# 0060 — Watch alerts in the user's language

Status: `done`

## Context

Plan 0058 made watch checks and alert texts code, so no status can be
invented and checking costs nothing. The texts are English only. A user who
talks to the assistant in Russian asked for a watch's alerts in Russian, and
the model could only answer that the alert has no prompt and its language
cannot be changed. Everything else the assistant writes follows the user's
language, so the alerts are the odd ones out.

## Goal

- A watch set up in another language sends its alerts, "back to normal"
  messages, and failure notices in that language.
- The facts stay the code's: the times, numbers, lines, platforms, and names
  in a translated alert are exactly those of the English text.
- An alert is never lost or late because of the translation: when it cannot
  be made, the English text goes out as before.

## Decisions

- **The setup model records the language.** A watch gets an optional
  `language` field, such as Russian. The model that proposes or changes the
  watch fills it with the language of the conversation, so the user needs no
  setting. It appears in the confirmation summary, and "English" or no
  language means the code text as is. Existing watches stay English until
  changed. The definition is JSON, so no migration is needed.
- **The model words; code checks.** Checks and the alert's facts stay in
  code. At delivery, an alert of a watch with a language is rewritten by one
  model call on the owner's own subscription. That call has no tools, no
  memory, no web search, and a fresh session. It does not take the owner's
  turn slot, so a running chat turn does not delay an alert.
- **Every number must survive.** The rewrite is used only when it is not
  empty, fits a Telegram message, and contains every number of the English
  text. Otherwise, or when the call fails or takes over 30 seconds, the
  English text is sent.
- **What the user saw is what the next turn hears.** The notice passed to
  the owner's next conversation turn carries the text that was sent.
- **Visible outcomes.** Each rewrite is counted by outcome (`rewritten`,
  `rejected`, `failed`, `timeout`).

## Approach

1. `watch.Spec.Language`, normalized and shown in `Describe`; the
   `language` tool argument with guidance on filling it; `ReadyWatchAlerts`
   returns the watch's language with each alert.
2. A `Rewriter` the watch service uses at delivery, with the number check
   and the English fallback; `conversation.Service.RewriteAlert` implements
   it on the owner's subscription.
3. Wiring in `internal/app`, the `home_agent.watch.rewrites` counter, and
   the README, `docs/architecture.md`, and `docs/operations.md`.

## Files affected

- `internal/watch` (spec, tools, delivery)
- `internal/store/sqlite/watches.go`
- `internal/conversation` (the rewrite call)
- `internal/observability/watches.go`
- `internal/app/app.go`
- README, `docs/architecture.md`, `docs/operations.md`

## Validation

- `go test -race ./...`, `go vet ./...`, formatting, build, and
  `scripts/check-plans`.
- Live check: change a watch to Russian in conversation, see the language in
  the confirmation, and compare its next alert with the NS app.

## Risks and open questions

- A rewrite adds a few seconds to an alert, and a slow model holds the watch
  loop's cycle for at most 30 seconds.
- The number check catches changed times and lines, not a translated station
  name; the prompt asks to keep names as they are.
- A transient Telegram failure after a rewrite makes the next cycle rewrite
  again.
