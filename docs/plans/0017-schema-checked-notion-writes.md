# 0017 — Schema-checked Notion writes and readable confirmations

Status: `in-progress`

## Context

[0011](0011-confirmed-notion-writes.md) made every Notion write a proposal the
user confirms in chat. A real use exposed three gaps at once.

A mentoring row was to move to the finished status. The model proposed
`Status: Completed`, the user confirmed, and Notion answered HTTP 400, which
the connector maps to "Notion rejected the request; nothing was written". The
model has no way to learn the option names a `status` property accepts:
`renderSchema` lists only property names and types, query results carry row
values, and no read tool retrieves a data source. Verified against Notion's
documentation on 2026-09-07: `GET /v1/data_sources/{data_source_id}` returns
the property schema with the `options` of every select, status, and
multi-select property; a status option that does not already exist cannot be
created through the API, and a select option is only created when the
connection may write the parent schema. An invented option name is therefore a
guaranteed rejection, discovered only after the user has confirmed.

The failure is also a dead end. Notion's own message is deliberately dropped
(`internal/connectors/notion/errors.go`), and the outcome of a confirmed
action never reaches the model at all — it only edits the proposal message. In
the recorded conversation the model then invented an explanation ("perhaps you
pressed Reject") and claimed a replacement proposal was pending when none had
been made.

Finally the proposal message reads as machine output: an English sentence with
a raw page UUID, the new value with no indication of the current one, and no
link to the page. Telegram messages are sent as plain text with no
`parse_mode`, so the model's own Markdown reaches the user as literal
asterisks.

## Goal

A property write is checked against the target's real schema before the user
is asked to confirm anything, so a wrong property or a non-existent option is
corrected by the model during the same turn instead of failing after a
confirmation. The confirmation message names the page as a link, shows the
current value beside the proposed one, and renders with real formatting. A
rejection that still happens explains itself in the user's terms without
quoting Notion.

## Decisions

- Add `GET /v1/data_sources/{id}` to the typed client. A data source's schema
  is read at propose time for `notion_update_page` on a data-source row and
  for `notion_create_page` under a data-source parent.
- Validate proposed properties against that schema before recording an
  approval, in the connector, not the model: unknown property name,
  computed or otherwise unwritable property, a value object whose key does not
  match the schema type, and a select, status, or multi-select option that
  does not exist are each a tool error naming the valid alternatives. The
  model sees it as a normal tool failure and can correct itself within the
  turn's call budget.
- Reject unknown select options as well, even though Notion may create them,
  so an assistant typo cannot silently add options to a household table.
- A schema that cannot be read (an unshared parent, an unavailable API) does
  not block the proposal: validation is skipped and the write proceeds as it
  does today. Availability of the write matters more than the check.
- Read tools expose structure so the model can inspect before proposing:
  `renderSchema` lists the options of select, status, and multi-select
  properties, bounded in length, and a page that lives in a data source
  renders its parent's schema.
- The proposal message is rewritten around the page, not the request: a
  header line, the page title as a link, one line per property showing
  `current → proposed`, and the expiry. The page UUID leaves the message; it
  stays in the stored approval record.
- Telegram messages are sent with `parse_mode=HTML`. A single conversion point
  in the client escapes text and renders a conservative Markdown subset —
  bold, inline code, fenced code, and links — leaving underscores untouched so
  identifiers such as `notion_update_page` survive. A message Telegram refuses
  to parse is resent once as plain text.
- A confirmed action that still fails validation-shaped (HTTP 400) is
  re-checked against the schema, and the user is told which property or option
  no longer matches. Notion's own message text stays out of logs, the model,
  and the chat, as decided in 0011.
- The resolution of a proposal is reported to the model: the approval service
  keeps the last resolved proposal per principal, and the conversation service
  prefixes a one-line note to the next turn. The note is in-memory only; a
  restart drops it, which is acceptable because the chat message already
  carries the outcome.

## Approach

1. Client and read rendering: add `DataSource`, render options in
   `renderSchema`, include the parent data source's schema when rendering a
   page that belongs to one, and add the retrieval route to `notionsim`.
2. Telegram formatting: add the Markdown-subset to HTML converter, send every
   message with `parse_mode=HTML`, and fall back to plain text once when
   Telegram rejects the entities.
3. Write proposals: read the target's schema at propose time, validate the
   proposed properties against it, render the new proposal layout with current
   values, and map a validation-shaped execution failure to a named cause.
4. Proposal outcome feedback: record the last resolution per principal and
   prefix it to the model's next turn.
5. Update the write guidance so the model reads the structure before proposing
   property changes, and record the plan and index row.

## Files affected

- `internal/connectors/notion/client.go`, `render.go`, `writes.go`, `tools.go`
- `internal/testutil/notionsim/simulator.go`
- `internal/telegram/client.go` and a new `internal/telegram/markup.go`
- `internal/approval/approval.go`
- `internal/conversation/service.go`, `types.go`
- `internal/connectors/notion/service_test.go`, `internal/telegram/*_test.go`
- `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go mod verify`, `go test ./...`,
  `go vet ./...`, `go build -buildvcs=false ./...`, `scripts/check-plans`.
- Simulator-backed tests: a status option that does not exist is refused with
  the valid options before any approval is recorded; a valid option proposes,
  confirms, and writes; a page whose schema cannot be read still proposes.
- Telegram integration tests assert `parse_mode=HTML`, escaped user content,
  a rendered link in the proposal, and the plain-text retry.
- Live check in the operator's own chat: move the mentoring row to the
  finished status end to end.

## Risks and open questions

- Propose time now costs one extra Notion request for a data-source row; the
  ten-second per-request deadline and the turn budget absorb it.
- HTML parse mode applies to every message; unescaped user or Notion content
  would break rendering, so escaping is centralized and covered by tests.
- The outcome note is best-effort in memory and is dropped by a restart.
