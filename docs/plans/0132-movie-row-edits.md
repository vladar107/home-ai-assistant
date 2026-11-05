# 0132 — Let the model edit existing movie rows

Status: `done`

## Context

Plan 0131 made `movies_add`, `movies_schedule`, and `movies_set_score` the only way to change the
movie table and told the model never to create or update its rows with `notion_create_page` or
`notion_update_page`. None of the movie tools changes an evening that is already in the table:
they cannot swap two films, put a different film on an evening, move a film into a reserved
evening, or change who picks an evening. Asked to change the films, the bot had no allowed tool
and the request failed.

## Goal

- Asking the bot to swap, replace, or move films between existing evenings, or to change an
  evening's picker or date, produces a `notion_update_page` proposal for those rows.
- Adding films keeps the turn order of plan 0131.

## Decisions

- **Updates are allowed, creates are not.** The guidance keeps `movies_add` and `movies_schedule`
  as the only way to add evenings, and `movies_set_score` for scores, but tells the model to edit
  existing rows with `notion_update_page`.
- **Swap films, not dates.** To exchange two films the model changes the `Фильм` column of both
  rows and leaves their dates alone, so each line of the proposal reads as one evening getting
  one film.
- No new tool and no change to the movie tools' behavior.

## Approach

1. Rewrite `movieWriteGuidance` in `internal/connectors/notion/movies.go` along the decisions.
2. Update the guidance test in `movies_test.go` to require the new wording and the absence of the
   old prohibition on updates.
3. Plan index, checks, deploy.

## Files affected

- `internal/connectors/notion/movies.go`, `movies_test.go`
- `docs/plans/0132-movie-row-edits.md`, `docs/plans/README.md`

## Validation

- Formatting, module, vet, test (with and without `-race`), build, and plan checks.
- After deployment, ask the bot to swap two upcoming films and confirm it proposes two
  `Фильм` updates.

## Risks and open questions

- A multi-row update batch can still be hard to check before confirming; keeping dates fixed and
  changing only `Фильм` limits that.
