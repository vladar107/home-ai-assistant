# 0075 — Tuning after the first conversation with jobs, backlog, and agent watches

Status: `done`

## Context

The first conversation after plans 0071–0074 went live showed four rough edges:

- **A job's facts were cut off.** The user asked which train to take home on
  Wednesday evening. The "Scheduled jobs" section shows the first 300
  characters of each job's instructions, and the Wednesday job names the
  Utrecht–Hoofddorp trip home only after that. The model guessed the wrong
  direction and asked the user instead of reading the job with `schedule_get`,
  and read it only after the user complained.
- **An all-day watch said "until midnight".** Confirming an agent watch that
  runs all day every day answered "Watching. Checking now until midnight.",
  which the user read as the watch ending at midnight.
- **An empty search looked like an outage.** The model called `web_search`
  with an empty query. The search check rejects an empty query as a bad
  request, which the search extension reports as "Web search is unavailable
  right now", so the model told the user search was down.
- **The tool hint suggested memory for a backlog request.** The model chose
  `backlog_add` anyway, but the hint pointed at `memory_add`.

## Goal

- Facts in a job's instructions reach the model, and it reads the job before
  asking the user about their plans.
- Confirming or changing an all-day watch says how often it checks, not when
  the day ends.
- An empty search query tells the model to give one.
- The backlog tool's description separates it from memory.

## Decisions

- **Longer excerpts, a larger section.** Each job's excerpt grows from 300 to
  800 characters, and the section's cap from 4000 to 10000, so a household's
  usual jobs fit whole or nearly. The section still changes only when a job
  does, so the prompt stays cacheable.
- **Read the job before asking.** The section's guidance adds: before asking
  the user about their plans, trips, or routine, read the job that covers them
  with `schedule_get`.
- **All-day confirmation text.** A home or agent watch whose window is the whole
  day answers "Checking all day, <days>, <how often>." Narrowed windows and
  transit watches keep their current text.
- **Empty queries are refused, not failed.** The search check answers a blank
  query with a deny decision whose text says the query was empty and nothing
  was searched, instead of a bad-request status. A malformed body stays a bad
  request. The empty query is not judged or counted.
- **Backlog description.** `backlog_add` describes itself as a feature or
  change to build later in a development session, not a fact or preference.

## Approach

1. `internal/schedule/prompt.go`: limits and guidance.
2. `internal/watch/tools.go`: the all-day confirmation text.
3. `internal/runtime/pi/bridge.go`: the empty-query decision.
4. `internal/backlog/tools.go`: the description.
5. Tests for each.

## Files affected

- `internal/schedule/prompt.go`
- `internal/watch/tools.go`
- `internal/runtime/pi/bridge.go`
- `internal/backlog/tools.go`
- tests in `internal/runtime/pi`, `internal/watch`, `internal/telegram`
- `docs/architecture.md`

## Validation

- An empty search query comes back to the model as "empty query", without
  running the check.
- Confirming an all-day agent watch says it checks all day, and how often.
- A job whose instructions run past 300 characters is listed with its later
  facts.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- A larger jobs section costs prompt tokens on every turn; it is cached, and
  the cap bounds it.
