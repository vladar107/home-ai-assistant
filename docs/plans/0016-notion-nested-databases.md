# 0016 — Reach data sources nested inside Notion pages

Status: `done`

## Context

[0010](0010-notion-workspace-connection.md) contributes `notion_search`,
`notion_get_page`, and `notion_query_data_source`, and states that a data
source ID comes from a search result. Production use found that this is the
only route to a data source, and it is not always available.

A page whose body holds an inline database renders its `child_database`
block as `[database] <title>` in `notion_get_page`, dropping the block ID
that `Block.ID` already carries. The model can therefore see that a table
exists and has no handle on it. `notion_search` finds the table only when
the query matches the table's own title, which differs from the containing
page's title and is often empty for an inline database. Observed on the
"Кино вечера" page: the model located the page, reported a nested table it
could not read, and a data-source search for the page title returned
nothing.

Verified against Notion's API documentation on 2026-09-06:
`GET /v1/databases/{database_id}` requires `Notion-Version: 2026-03-11`,
returns `object: "database"` with `title`, `is_inline`, `parent`, and a
`data_sources` array whose entries each carry `id` and `name`, and is the
documented migration path from a database ID to a data source ID. A
`child_database` block's ID is that database ID. Search returns data source
objects rather than database objects, and a database with several data
sources appears as one search result per data source.

## Goal

When a page contains a table, the model can read its rows. Following the
page's own content is enough: no search by the table's title, and no
knowledge of the database and data source distinction.

## Decisions

- `child_database` and `child_page` blocks render with their block ID, the
  way every other object rendering in the connector already names an ID.
  A `child_database` line states that the ID is a database ID so the model
  passes it where it belongs.
- `notion_query_data_source` accepts a database ID in `data_source_id`
  instead of gaining a second parameter or a sibling tool. The model has one
  way to read table rows, and the database and data source split stays
  inside the connector.
- Resolution is explicit rather than a retry of a failed query: an ID that
  Notion reports as not a data source is looked up with
  `GET /v1/databases/{id}`, and the query is repeated against its single
  data source. A database with several data sources is not guessed; the
  tool returns the data source names and IDs and asks for one of them.
- A database ID that is neither a data source nor a readable database keeps
  the existing not-shared error text. The resolution hop adds one request
  and stays inside the tool call's existing deadline and retry policy.
- No new capability, grant, or risk class. This is read-only navigation of
  content the connection is already shared with.

## Approach

1. Add `Database` to the typed client: `GET /v1/databases/{id}` decoded into
   an object carrying the title, `is_inline`, and the `data_sources` list of
   ID and name pairs. Errors classify through the existing mapping.
2. Render `child_database` as `[database] <title> (database id: <id>)` and
   `child_page` as `[page] <title> (id: <id>)`, using the block ID the
   renderer already receives.
3. In `notion_query_data_source`, run the data source query first. When
   Notion rejects the ID as not a data source, retrieve the database, and
   query its single data source; with several, return the list to choose
   from without querying.
4. Update the tool descriptions and guidance so the ID may come from a
   search result or from a `child_database` line in page content.
5. Extend the Notion simulator with `GET /v1/databases/{id}`, databases that
   own one or several data sources, and pages holding a `child_database`
   block, so the path is covered end to end.
6. Update `docs/04-integrations.md` for the widened `notion_query_data_source`
   input and the navigable page content.

## Files affected

- `internal/connectors/notion/client.go`, `render.go`, `tools.go`
- `internal/testutil/notionsim/simulator.go`
- `internal/connectors/notion/service_test.go`
- `docs/04-integrations.md`, `docs/plans/README.md`

## Validation

- Formatting, module, vet, build, race-detected tests, and `scripts/check-plans`.
- An integration test where a page holds an inline database: the model reads
  the page, takes the database ID from the rendered content, calls
  `notion_query_data_source` with it, and receives the rows.
- An integration test where a database owns two data sources: the tool
  returns both names and IDs and queries neither.
- An integration test that a data source ID still queries directly, with one
  request and no database lookup.
- On the NAS, ask for the rows of the table nested in the "Кино вечера"
  page and confirm the model answers from the table's own entries.

## Risks and open questions

- A database ID and a data source ID are indistinguishable by shape, so the
  first query of a database ID costs a rejected request. Accepted: it keeps
  one tool and one parameter, and the rejection is cheap and classified.
- A table rendered as a linked view of a database living elsewhere is not
  covered by this plan; its block is not a `child_database`. Left for a
  later plan if it appears in practice.
- Notion's own row limit and the broker's result cap still apply, so a large
  table is truncated the way a large page already is.
