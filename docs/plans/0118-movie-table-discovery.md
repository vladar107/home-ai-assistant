# 0118 — movies_next finds the movie table

Status: `done`

## Context

The movie tools (`movies_next`, `movies_set_score`, `movies_schedule`) in the
Notion connector require `data_source_id`, which the model must first find with `notion_search`.
The weekly review filed a backlog item after `movies_next` failed twice in a week. Both failures were
calls with no `data_source_id`: once in a scheduled brief that never searched, and once in a chat
where the following `notion_search` used a query that matched no title, so the model told the user
the table could not be found. The table was shared all along; asking again worked.

The movie table is already recognized by its shape, a title column of dates and a `Фильм` text
column, and the search API returns each data source's schema, so the application can find the
table itself instead of depending on the model's search query.

## Goal

`movies_next` called without `data_source_id` finds the movie table among the data sources the
user shared with their Notion connection and answers as if the ID had been passed. A call with an
explicit ID behaves as today.

## Decisions

- Only `movies_next` makes `data_source_id` optional. The writes keep requiring it: they did not
  fail, and a write that found the table itself would skip the outside-content read that today
  turns it into a proposal, so they would start writing at once where they now ask.
- The table is found by schema, not title: shared data sources from the search API with the
  data-source object filter, kept when they have a property named `Фильм`, the check `movieTable`
  already applies to an explicit ID (every data source has a title column). Trashed results are
  skipped.
- Exactly one match is used. None returns an error saying no shared data source looks like the
  movie table and that the user must share it with the Notion connection. Several returns an
  error listing each candidate's title and ID so the model can pass one; `movies_next` is a
  third-party tool, so its errors already reach the model framed as untrusted data.
- The listing reads at most three pages of 100 results; a household shares far fewer data sources,
  and a match found before that bound is used.
- The lookup returns only the ID, and the existing table loading runs unchanged.
- A discovered table's ID ends the `movies_next` answer, so a following score or schedule write has
  the ID it still requires without a search.
- Guidance tells the model to call `movies_next` without `data_source_id` unless it already has
  the table's ID.

## Approach

1. Extract the schema check in `movieTable` into a helper used by both paths.
2. Add the lookup in `movies` when `movies_next` has a blank `data_source_id`, before loading the
   table, with Notion failures surfacing through the existing error mapping. The writes keep
   their "data_source_id is required" error.
3. Drop `data_source_id` from the `movies_next` schema's `required` list, describe the omission in
   its property description, and update `movieGuidance`.
4. Add integration tests with the Notion simulator: a call without an ID finds the shared table
   beside a shared non-movie data source; only the non-movie one shared, and two lookalikes, each
   return their error. The existing movie tests, which pass an explicit ID, stay unmodified.

## Files affected

- `internal/connectors/notion/movies.go`
- `internal/connectors/notion/movies_test.go`
- `docs/plans/0118-movie-table-discovery.md`, `docs/plans/README.md`

## Validation

- Formatting, module, vet, test (with and without `-race`), build, and plan checks.
- After deployment, ask the bot what film is next without mentioning the table and confirm the
  `movies_next` call succeeds without a prior `notion_search`.

## Risks and open questions

- A second shared table with a `Фильм` column, such as a copy or a watch list, makes the lookup
  ambiguous; the error names both so the model or user can choose.
- The writes still need an ID from an earlier result; if they turn out to fail the same way, a
  later plan can give them the lookup together with an explicit decision on confirmation.
