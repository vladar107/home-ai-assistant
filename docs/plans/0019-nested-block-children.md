# 0019 — Read nested block content through a block-children tool

Status: `in-progress`

## Context

[0016](0016-notion-nested-databases.md) reached data sources nested inside
pages, so a `child_database` block prints its ID and
`notion_query_data_source` reads its rows. A real use on 2026-09-09 exposed a
different gap with the same symptom.

`flattenBlocks` renders exactly one level. A block that has children is marked
`(has nested content)` and is never fetched, and the annotation does not carry
the block's ID. A Notion simple table is a `table` block whose `table_row`
children hold every cell, so a page containing one renders as
`[table] (has nested content)`. `renderBlock` already renders `table_row` as a
pipe-delimited row, but the rows are unreachable.

The read tools are `notion_search`, `notion_get_page`, and
`notion_query_data_source`. None accepts a block ID, and `BlockChildren` is
called only with the page ID, so the model cannot drill down. Asked which film
was next on a page holding a simple table, it correctly reported that it could
not read the table and asked for a screenshot.

The same gap hides the content of toggles, column layouts, and synced blocks.

## Goal

The model can read the children of any block it can see, one level per call,
because every nested block is printed with an ID it can pass to a read tool. A
simple table's rows become readable without a table-specific code path.

## Decisions

- Add one read tool, `notion_get_block_children`, taking only `block_id`. Its
  risk stays `tools.RiskRead`. It carries no `cursor` or `limit`, unlike the
  list-shaped read tools: the shared page-content helper already walks Notion's
  cursors internally and stops at `maximumContentBytes`, reporting truncation,
  so a second pagination control would contradict it.
- Reuse the existing pipeline rather than adding a parallel one. Notion treats
  a page as a block, so `BlockChildren` and `flattenBlocks` already accept any
  block ID; the page-content helper is generalized to take an arbitrary ID and
  both callers share it, including the `maximumContentBytes` bound and the
  truncation flag.
- `flattenBlocks` prints the ID beside the nested-content annotation. This is
  the whole reason the tool is usable: without an address the model can see
  that content exists but cannot ask for it.
- Descending stays explicit. `notion_get_page` does not fetch children on its
  own, which keeps a page render bounded and predictable and matches how
  `[database]` lines already require a second call.
- `readGuidance` gains one sentence naming the tool and stating that a simple
  table's rows are the children of its `table` block, so the model does not
  have to infer the shape of Notion's table blocks.
- No write path, approval, or schema-validation behavior changes.

## Approach

1. Generalize the page-content helper in `internal/connectors/notion/tools.go`
   to any block ID, then add the tool constant, definition, and dispatch case.
   Complexity: simple.
2. Include the block ID in the nested-content annotation in
   `internal/connectors/notion/render.go`. Complexity: simple.
3. Extend `readGuidance` with the tool and the simple-table shape.
   Complexity: simple.
4. Teach `internal/testutil/notionsim/simulator.go` to serve children for
   nested block IDs and to mark a parent block `has_children`, and give a
   fixture page a simple table with rows. Complexity: moderate.
5. Cover it in `internal/connectors/notion/service_test.go`: page content
   prints the table's block ID, and the tool returns the rows as pipe lines.
   Complexity: simple.

## Files affected

- `internal/connectors/notion/tools.go`,
  `internal/connectors/notion/render.go`
- `internal/testutil/notionsim/simulator.go`
- `internal/connectors/notion/service_test.go`

## Validation

- `gofmt`, `go mod tidy -diff`, `go vet ./...`, and `go test ./...` pass.
- Through the simulator: a page holding a simple table renders a `[table]`
  line carrying a block ID, `notion_get_block_children` with that ID returns
  the rows, an unknown block ID fails as an ordinary tool error, and a page ID
  passed to the tool returns the page's own blocks.
- Live: asking which film is next on the "Кино вечера" page answers from the
  table instead of asking for a screenshot.
- `scripts/check-plans` passes.

## Risks and open questions

- Each nesting level costs one more model call, so deeply nested content can
  exhaust a turn's call budget. The connector does not bound depth; the turn
  budget already does.
- Block IDs become visible in rendered page content. They are opaque
  identifiers for data the user already shared with the connection, like the
  database IDs printed today.
- The tool accepts a page ID as well, because Notion treats pages as blocks.
  That is harmless and equivalent to `notion_get_page` with content.
