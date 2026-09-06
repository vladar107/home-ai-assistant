# 0011 — Confirmed Notion writes through core approvals

Status: `proposed`

## Context

[0010](0010-notion-workspace-connection.md) connects a token that may carry
update and insert capabilities but contributes read tools only. The user
wants the model to create and update Notion content as well, with a clear
separation between read-only and read-write access. The accepted module
model (`docs/12-module-and-automation-model.md`) places such writes at the
confirmed-action level: an exact action bound to a named, expiring,
same-principal approval, executed by the core rather than by the model. The
approval state machine of `docs/05-security-and-privacy.md` does not exist
yet.

## Goal

A user can switch Notion writes on or off. When on, the model may propose a
bounded write; the bot shows the exact change, the same user confirms or
rejects it, and the application executes a confirmed proposal exactly once
and reports the result. When off, or for a user who never switched it on,
the model has no write tool at all.

## Decisions

- A writes toggle button in the Notion section of `/settings` sets the
  writes flag on the principal's connection row, and the section shows it.
  The flag is the grant for
  the `write` risk class, so the broker offers write tools only while it is
  on, and rechecks it before executing.
- Write tools are proposals. `notion_create_page` (parent page or data
  source ID, title, optional properties, optional body paragraphs),
  `notion_update_page` (page ID, properties to set), and
  `notion_append_blocks` (page ID, paragraphs). Executing one records an
  approval and returns "proposed, awaiting the user's confirmation" to the
  model; nothing reaches Notion during the turn.
- Core approval records live in a new `approvals` table: ID, principal, tool,
  canonical arguments, hash of the canonical form, rendered summary, status
  (pending, confirmed, rejected, expired, executed, failed), created,
  expires, and finished timestamps. One pending approval per principal; a
  new proposal replaces the previous one; pending approvals expire after
  ten minutes and do not survive a restart unresolved.
- After the turn's reply, the bot sends the rendered proposal with Confirm
  and Reject buttons. Confirm from the same principal moves the record to
  confirmed in one transaction, executes the write with that principal's
  token, and edits the proposal message with the result and page URL; a
  rejected or expired proposal is reported in the same message. Buttons of
  a resolved or replaced proposal only re-render its final state.
- Execution never retries a create on an ambiguous failure; the record is
  marked failed with a content-free reason and the user is told to check
  Notion before trying again. Updates are retried once on a transient error.
- Property values are passed through as Notion property value objects for
  Notion to validate; the rendered summary shows property names and
  best-effort text values, falling back to compact JSON, so the user
  confirms what will actually be sent.
- Approval creation, expiry, and resolution are core services in a new
  `internal/approval` package; the Notion connector only renders and
  executes its own actions.

## Approach

1. Approval package and migration: records, canonical hashing, expiry,
   single-pending rule, and the store methods. Complexity: complex.
   Prerequisite for the rest.
2. After step 1, two parallel work areas. Complexity: complex.
   - Connector writes: create page, update page, append blocks, rendering
     of proposals, and simulator support for the write endpoints. Owns
     `internal/connectors/notion/`.
   - Telegram and broker: the writes toggle in the settings section, the
     proposal message and its buttons, proposal delivery after the turn,
     and write-class gating in the broker.
     Owns `internal/telegram/`, `internal/tools/`, and
     `internal/conversation/`.
3. Wiring, README, and the suites below.

## Files affected

- new `internal/approval/` and a migration in `internal/store/sqlite/`
- `internal/connectors/notion/`, `internal/tools/`,
  `internal/conversation/`, `internal/telegram/`, `internal/app/`
- `README.md`, `docs/04-integrations.md`

## Testing

Integration suites over the loopback simulators, real SQLite, and the
controllable runtime, following plan 0003.

- Writes off: a connected principal's turn carries no write definition, and
  a write call injected through the broker is rejected without a record.
- Writes on: the turn carries the write definitions; a `notion_create_page`
  call creates a pending record and reaches no simulator endpoint; the bot
  sends the rendered proposal after the reply; Confirm executes exactly one
  request with the principal's token and edits the message with the result;
  Reject and expiry leave Notion untouched and report accordingly; a second
  proposal replaces the first; a Confirm press from another principal is
  ignored; a restart expires a pending record.
- Ambiguous create failure marks the record failed and sends no second
  request; a transient update failure is retried once.
- Owns `internal/telegram/*_test.go` and
  `internal/connectors/notion/*_test.go`.

## Validation

- All suites pass under `go test ./...` and `go test -race ./...`, with the
  configured formatting, module, vet, build, and plan checks.
- Live: switch writes on, ask the model to add a task to a shared data
  source, confirm, and verify the page in Notion; reject a second proposal
  and verify nothing changed; switch writes off and confirm the model no
  longer offers to write.

## Risks and open questions

- Confirmation adds a round trip to every write; promote individual
  operations to the bounded-write level only after they have proven
  idempotent and reversible.
- The model may describe the proposal differently from the rendered
  summary; the summary, not the model's words, is what the user confirms.
- Rich property types, file uploads, and moving or deleting pages are out of
  scope.
