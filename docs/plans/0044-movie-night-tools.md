# 0044 — Movie night tools for the Notion movie table

Status: `in-progress`

## Context

The household keeps its movie nights in a Notion database on the "Кино вечера"
page (converted from a simple table in 0019). Every row is one evening:

| Property | Type      | Meaning                                         |
|----------|-----------|-------------------------------------------------|
| `Дата`   | title     | Evening date as text, `DD.MM.YYYY`              |
| `К`/`K`  | rich_text | Who picked the film: `К` or `В`                 |
| `Фильм`  | rich_text | Film title                                      |
| `В`      | rich_text | Score from the user whose name starts with В    |
| `K`/`К`  | rich_text | Score from the user whose name starts with К    |

Notion forbids duplicate property names, so the picker column and the second
score column both look like "K" but one is Latin and one is Cyrillic.

The model only reaches this table through the generic Notion tools, and it
gets three things wrong or makes them laborious:

- A score is written to whichever column the model guesses, not to the column
  of the user who gave it.
- New evenings need dates that continue the schedule, normally one week after
  the latest one, filled into the undated rows from top to bottom. The model
  does that date arithmetic over text titles by itself.
- "What's the next movie?" requires reading the whole table and comparing
  text dates with today.

## Goal

In ordinary conversation each user can:

- say "I give Кит an 8" and get a proposal that writes `8` into *their own*
  score column of that row;
- say "schedule the new films" and get one proposal per undated row, top to
  bottom, each one week after the previous, or give a specific date for one
  film;
- ask "what are we watching next?" and get the film whose date is the
  earliest on or after today in `HOME_AGENT_TIMEZONE`, with who picked it.

## Decisions

- **Typed tools, Notion-backed.** Three tools join the Notion connector, offered
  only while Notion is connected (and the two writes only while writes are on):
  `movies_next`, `movies_set_score`, `movies_schedule`. Each takes the movie
  table's `data_source_id` (a database ID is accepted, as in
  `notion_query_data_source`), so nothing is hard-coded and each user's
  connection reaches only what that user shared. The model finds the ID once
  with `notion_search`.
- **The table is recognized by shape, not by configuration.** The title
  property holds the date, and the film column is a rich_text property named
  `Фильм`. A table without these is rejected with a message naming the expected
  shape.
- **The score column is inferred from the user's display name.** The first
  letter of the principal's configured display name is folded to a canonical
  form: Latin and Cyrillic look-alikes (`K`/`К`, `B`/`В`) match, and a Latin
  initial is also transliterated (`V` → `В`). The candidates are rich_text
  properties whose single-letter name folds to that initial. When two remain
  (the picker column and a score column), the score column is the one whose
  filled values are numbers. No match, or an unresolvable tie, is an error
  naming the candidate columns, so nothing is ever written to a guessed column.
  The display name reaches the connector through `notion.Config` as a
  principal-ID → display-name map built from the configured Telegram users.
- **Scores stay text.** The columns are rich_text, so a score is written as its
  text (`8`, `7.5`). Accepted values are 0–10 in steps of 0.5. Rewriting an
  existing score is allowed; the proposal shows the old and new values.
- **Rows are matched by film title.** `movies_set_score` takes a `film` string,
  matched case-insensitively against `Фильм` after trimming. Several matches
  pick the most recent dated one and say so; none is an error listing the
  closest titles.
- **Dates continue from the latest date in the table.** The anchor is the
  maximum parseable `Дата` across all rows, whatever their order. Undated rows
  (empty or unparseable title, with a film set) get anchor + 7 days, anchor +
  14 days, and so on, from top to bottom. A specific date for one film
  overrides this and does not move the others.
- **"Top to bottom" means creation order.** The Notion API exposes neither a
  view's sort nor its manual drag order, so undated rows are ordered by
  `created_time` ascending. That matches the table as long as new films are
  added at the bottom.
- **Next movie: earliest date on or after today.** "Today" is the local date in
  `HOME_AGENT_TIMEZONE`, so tonight's film is still "next" until midnight. A row
  with a date but no film is reported as an evening without a film yet.
- **Writes reuse the existing approval path.** The write tools compute the
  property values and record ordinary `notion_update_page` proposals, so
  confirmation, schema checks, execution, and failure handling are unchanged.
  Several date proposals in one turn are confirmed together as a batch (0042).

## Approach

1. Add `movies.go` to the Notion connector: reading the table (one query
   following cursors, sorted by `created_time`), date parsing and formatting
   in `DD.MM.YYYY`, initial folding and score-column resolution, and the three
   tools with their guidance.
2. `movies_next` returns the film, its date and weekday, who picked it, and the
   row link; with nothing upcoming it says so and names the latest past evening.
3. `movies_set_score` resolves the row and the user's column and proposes a
   `notion_update_page` setting that column's rich_text.
4. `movies_schedule` takes an optional `film` and optional `date`
   (`YYYY-MM-DD`). Without a film it proposes dates for every undated row; with
   a film and no date it gives that row anchor + 7 days; with a date it uses
   that date.
5. Pass the principal display-name map and the time zone into `notion.Config`
   from `internal/app`.
6. Extend the Notion simulator only as far as these tests need
   (`created_time`, rich_text values).

## Files affected

- `internal/connectors/notion/movies.go` (new): table reading, column
  resolution, the three tools
- `internal/connectors/notion/service.go`, `tools.go`, `writes.go`: display
  names, zone, and clock in `Config`; contribution and dispatch; a shared
  proposal recorder
- `internal/app/app.go`: wiring
- `internal/testutil/notionsim/simulator.go`: `created_time` sorting
- `internal/connectors/notion/movies_test.go` (new), `service_test.go`, and the
  Telegram Notion integration tests, whose tool lists gain the movie tools
- `docs/plans/README.md`

## Validation

- Integration tests through the Notion simulator: score lands in the caller's
  column with Latin/Cyrillic look-alike headers and a picker column of the same
  letter; ambiguous or missing columns fail without a proposal; undated rows get
  consecutive weekly dates in creation order from the table's latest date; an
  explicit date is used as given; next movie at the day boundary in the
  configured zone.
- `gofmt`, `go mod tidy` check, `go vet`, `go test ./...`, `go build`,
  `scripts/check-plans`.
- Live: on the real table, ask for the next movie, give a score as each user,
  and schedule a new film, confirming each proposal.

## Risks and open questions

- Display names such as `Vlad` and `Kim` fold to `В` and
  `К`. Renaming a user to a different initial breaks the match; it then fails
  closed with an error rather than writing elsewhere.
- Creation order differs from the visible order if a row was dragged or
  inserted in the middle. The proposals show every date before anything is
  written, so a wrong order is caught at confirmation.
- Filling the picker column is out of scope: the user decided against it.
