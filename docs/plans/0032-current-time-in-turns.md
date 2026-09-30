# 0032 — Tell the model the current local time on every chat turn

Status: `done`

## Context

The model has no clock. It falls back to its training cutoff when a user says "today",
"tomorrow", or "next Friday", or asks how long until something. Scheduled turns already name
the time they run for; ordinary chat turns carry nothing.

`HOME_AGENT_TIMEZONE` (default `Europe/Amsterdam`) is the household's zone, used so far only
by scheduled jobs.

## Goal

Every chat turn tells the model the current local date, weekday, time, and zone, so relative
dates and times resolve correctly.

## Decisions

- **In the message, not the system prompt:** the time is a `[system]` line prepended to the
  turn's message, like tool hints and approval notices. A changing system prompt would break
  prompt-cache reuse of the whole session on every turn; in the message it also leaves each
  earlier message's send time in the session history.
- **One household zone:** `HOME_AGENT_TIMEZONE` applies to every user. Per-user zones can come
  later if a user lives elsewhere.
- **Scheduled turns unchanged:** they already state the time they run for.

## Approach

1. Add an optional `Location` to the conversation service's configuration; when set, the turn
   message starts with `[system] Current local time: Wednesday 30 September 2026, 14:05 CEST
   (Europe/Amsterdam).`
2. Give the service an injectable clock (`Now`, defaulting to `time.Now`), as the scheduler
   has, so tests can pin the time.
3. Load `HOME_AGENT_TIMEZONE` in the application wiring and pass it in.

## Files affected

- `internal/conversation/types.go`, `internal/conversation/service.go`
- `internal/app/app.go`, `internal/config/config.go`
- `CLAUDE.md`: a product rule that every turn carries the current local time
- `internal/telegram/conversation_integration_test.go`, `internal/telegram/time_integration_test.go`

## Validation

- End-to-end test from a Telegram update to the runtime: the exact time line on a summer-time
  turn, then a fresh reading with the winter offset on the next turn of the same session.
- Formatting, vet, build, the full test suite, and `scripts/check-plans`.
- Live: ask the assistant what day it is after the operator restarts the GoLand server.

## Risks and open questions

- The line adds roughly twenty tokens to each turn.
