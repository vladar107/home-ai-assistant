# 0049 — Tell the model who the user is

Status: `done`

## Context

The model is never told which household member it is talking to. Each user's
display name is configured beside their Telegram ID, but only the Notion
connector reads it, to find the user's score column for `movies_set_score`.
Chat, task, and scheduled turns carry the current time and the user's location,
but no name, and the system prompt is shared on purpose so it can be cached.

On 2026-10-04 Kim asked "Какую оценку я поставила Киту?". The model read
the movie table with `notion_query_data_source`, saw the single-letter score
columns В and К, and answered that she gave 9 and "К" gave 9 too. It had no way
to know that К is her own column.

## Goal

Every turn names its user to the model, so "I", "me", and "my" resolve to the
right household member. In particular, reading the movie table answers with the
user's own score.

## Decisions

- **The name goes in the turn's message, not the system prompt.** It goes in
  as a `[system] The user is <display name>.` line, next to the time line, so
  the prompt stays cacheable and stays the same for every user. Chat, task, and
  scheduled turns all carry it.
- **The configured display name is the name.** It is the same value
  `movies_set_score` maps to a column, so the reading and writing sides agree.
  A principal without a display name gets no line.
- **The movie guidance states the column rule.** A single-letter numeric
  column holds the score of the household member whose name starts with that
  letter, and Latin and Cyrillic forms of a letter count as the same. With the
  name on the turn, the model can find the user's own column when it reads rows.
  A dedicated read tool for scores is left for later, in case this is not
  enough.

## Approach

1. Add `DisplayNames` (principal ID → display name) to `conversation.Config`
   and fill it in `app.go` from the Telegram users, as the Notion config
   already is.
2. Prefix the user line in chat turns (`turnMessage`), task runs (`RunTask`),
   and scheduled turns (`RunScheduled`).
3. Extend `movieGuidance` with the score-column rule.

## Files affected

- `internal/conversation/types.go`, `service.go`, `task.go`, `scheduled.go`
- `internal/app/app.go`
- `internal/connectors/notion/movies.go`

## Validation

- Formatting, module, vet, test, build, and plan checks.
- Live: Kim asks for her own score of a film from the movie table and gets
  her column's value.

## Risks and open questions

- If the model still misreads columns when there are several single-letter
  columns, add a `movies_scores` read tool that labels scores by person.
