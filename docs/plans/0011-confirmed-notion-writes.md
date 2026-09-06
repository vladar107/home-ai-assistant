# 0011 — Confirmed Notion writes through core approvals

Status: `done`

## Context

[0010](0010-notion-workspace-connection.md) connects a token that may carry
update and insert capabilities but contributes read tools only. The user
wants the model to create and update Notion content as well, with a clear
separation between read-only and read-write access. The accepted module
model (`docs/12-module-and-automation-model.md`) places such writes at the
confirmed-action level: an exact action bound to a named, expiring,
same-principal approval, executed by the core rather than by the model. The
approval state machine of `docs/05-security-and-privacy.md` did not exist
yet.

Verified against Notion's API documentation on 2026-09-06: `POST /v1/pages`
takes a `page_id` or `data_source_id` parent, page-parent properties accept
only `title`, data-source properties are keyed by property name, and
`children` holds up to 100 blocks; `PATCH /v1/pages/{id}` sets `properties`;
`PATCH /v1/blocks/{id}/children` appends up to 100 blocks and needs the
insert content capability.

## Goal

A user can switch Notion writes on or off. When on, the model may propose a
bounded write; the bot shows the exact change, the same user confirms or
rejects it, and the application executes a confirmed proposal exactly once
and reports the result. When off, or for a user who never switched it on,
the model has no write tool at all.

## Decisions

- A writes toggle button in the Notion section of `/settings` sets the
  writes flag on the principal's connection row, and the section shows it.
  The flag is the grant for the `write` risk class: the connector
  contributes write tools only while it is on, the broker rechecks the
  contribution before every call, and the executor checks the flag again
  before a confirmed action runs. A reconnect resets the flag.
- Write tools are proposals. `notion_create_page` (parent page or data
  source ID, title, optional properties, optional body paragraphs),
  `notion_update_page` (page ID, properties to set), and
  `notion_append_blocks` (page ID, paragraphs). Executing one validates and
  re-encodes the arguments, reads the target page's title for update and
  append so an unshared page fails before anything is proposed, records an
  approval, and returns "proposed, awaiting the user's confirmation" to the
  model; nothing is written during the turn.
- Core approval records live in a new `approvals` table: ID, principal, tool,
  canonical arguments, hash of the canonical form, rendered summary, status
  (pending, confirmed, rejected, expired, executed, failed), reason,
  outcome, chat and message of the proposal message, created, expires, and
  finished timestamps. One pending approval per principal, enforced by a
  partial unique index; a new proposal replaces the previous one and reports
  that in its message; pending approvals expire after ten minutes, swept
  every thirty seconds; at start every pending record expires and every
  confirmed one is marked failed as interrupted.
- After the turn's reply, the coordinator's follow-up hook sends the
  rendered proposal with Confirm and Reject buttons and stores the message
  ID; an undeliverable proposal is retried by the sweep until it expires.
  Confirm from the same principal moves the record to confirmed in one
  statement, executes the write with that principal's token within
  forty-five seconds, and edits the proposal message with the result and
  page URL; a rejected, expired, or replaced proposal is reported in the
  same message. Buttons of a resolved or replaced proposal only re-render
  its final state; a press from another principal is ignored.
- Execution never retries a create or an append after an ambiguous failure;
  the record is marked failed with a content-free reason and the user is
  told to check Notion before trying again. Updates are retried once on a
  transient error. Notion's rejections map to short reasons and sentences
  without provider text.
- Property values are passed through as Notion property value objects for
  Notion to validate; the rendered summary shows property names and
  best-effort text values, falling back to compact JSON, so the user
  confirms what will actually be sent.
- Approval creation, expiry, and resolution are core services in a new
  `internal/approval` package; the Notion connector only renders and
  executes its own actions through narrow interfaces, and the Telegram
  service routes `approval:` button presses to the core.

## Approach

1. Approval package and migration: records, canonical hashing, expiry,
   single-pending rule, restart handling, and the store methods.
2. After step 1, two parallel work areas.
   - Connector writes: create page, update page, append blocks, rendering
     of proposals, approved execution, and simulator support for the write
     endpoints.
   - Telegram, coordinator, and client: the writes toggle in the settings
     section, the proposal message and its buttons, proposal delivery after
     the turn, and the approval callback route.
3. Wiring, README, integrations and runbook documentation, and the suites
   below.

## Files affected

- new `internal/approval/` and migration `007_approvals.sql` with
  `internal/store/sqlite/approvals.go`
- `internal/connectors/notion/`, `internal/integration/`,
  `internal/conversation/`, `internal/telegram/`, `internal/app/`,
  `internal/testutil/notionsim/`
- `README.md`, `docs/04-integrations.md`, `docs/13-operator-runbook.md`

## Testing

Integration suites over the loopback simulators, real SQLite, and the
controllable runtime, following plan 0003.

- Writes off: a connected principal's turn carries no write definition, and
  a write call injected through the broker is rejected without a record.
- Writes on: the turn carries the write definitions and guidance; a
  `notion_create_page` call creates a pending record and reaches no
  simulator endpoint; the bot sends the rendered proposal after the reply;
  a press from another principal is ignored; Confirm executes exactly one
  request with the principal's token and edits the message with the result
  and URL; a stale press re-renders the outcome without a second request;
  Reject leaves Notion untouched; a second proposal replaces the first and
  both messages report it; writes off removes the tools again.
- Expiry edits the proposal message and a later press re-renders it; a
  restart expires a pending record and reports it; an ambiguous create
  failure marks the record failed with one request sent; a transient update
  failure is retried once. Tokens and proposal content never appear in logs.
- Owns `internal/telegram/approval_integration_test.go` with the simulator
  in `internal/testutil/notionsim/`.

## Validation

- All suites pass under `go test ./...` and `go test -race ./...`, with the
  configured formatting, module, vet, build, and plan checks.
- Live: switch writes on, ask the model to add a task to a shared data
  source, confirm, and verify the page in Notion; reject a second proposal
  and verify nothing changed; switch writes off and confirm the model no
  longer offers to write. The runbook lists these steps; they were not yet
  performed when this plan was committed.

## Risks and open questions

- Confirmation adds a round trip to every write; promote individual
  operations to the bounded-write level only after they have proven
  idempotent and reversible.
- The model may describe the proposal differently from the rendered
  summary; the summary, not the model's words, is what the user confirms.
- A confirmed action runs inside the Telegram poller, so a slow Notion
  write delays other updates by up to its deadline.
- Rich property types, file uploads, and moving or deleting pages are out of
  scope.
