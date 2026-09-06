# 0007 — Settings command with inline controls

Status: `done`

## Context

Seven commands are registered and the model connection alone takes three of
them: `/connect`, `/connection`, and `/disconnect`. The Notion plans
[0010](0010-notion-workspace-connection.md) and
[0011](0011-confirmed-notion-writes.md) would add connect, disconnect, a
writes switch, and confirm and reject, which is how a command list stops
being a set of shortcuts. `docs/04-integrations.md` lists inline
confirmation and cancellation buttons as a Telegram primitive, but the
poller currently requests message updates only and has no callback
handling.

## Goal

One `/settings` command renders every per-user setting in a single message
with inline buttons for each action. The model connection commands fold into
it, and later integrations add a section rather than commands. `/status`,
`/privacy`, `/cancel`, and `/new` remain as operational controls.

## Decisions

- The poller requests `message` and `callback_query` updates. A callback is
  authorized like a message: the numeric `from.id` must map to an enabled
  principal and the message it belongs to must be in that principal's
  private chat; anything else is ignored with the existing reasons and never
  answered. Callback data is untrusted input: a short action from a fixed
  vocabulary, and unknown or stale actions only re-render the view.
- Every accepted callback is answered with `answerCallbackQuery`, with a
  short toast where it helps, and the settings message is updated in place
  with `editMessageText` and a fresh keyboard before the cursor advances. An
  unchanged edit counts as success; an edit rejected because the message is
  too old sends a new settings message instead. Replies that need their own
  message, such as connection instructions, are sent as plain messages
  because a button press has no message to quote.
- Settings are composed from sections. A section renders its lines and
  buttons for a principal and handles its own actions; the Telegram service
  composes sections in a fixed order and owns delivery. This plan ships the
  model subscription section: connection state with the account suffix and
  expiry, and a Connect or Disconnect button. A privacy line remains
  informational. The Notion section arrives with plan 0010.
- `/connect`, `/connection`, and `/disconnect` are removed from registration
  and handling; `/settings` is registered; the help text and the
  not-connected reply point to `/settings`.
- Telemetry distinguishes message and callback updates; callback data and
  message text are not logged.

## Approach

1. Callback support: update and callback types, `answerCallbackQuery`,
   `editMessageText`, keyboards on send, poller authorization and cursor
   handling for callbacks, and simulator support. Complexity: complex.
2. After step 1, the settings view: section interface, renderer, keyboard
   building, the model subscription section replacing the three commands,
   and the registered command list. Complexity: moderate.
3. Tests, README, and the command list in `docs/04-integrations.md`.

## Files affected

- `internal/telegram/` (client, service, commands, new settings file, tests
  and simulator)
- `README.md`, `docs/04-integrations.md`

## Testing

Integration suites over the loopback Telegram simulator, real SQLite, the
loopback provider simulator, and the controllable runtime, following plan
0003.

- `/settings` renders the model line and the Connect button for a
  disconnected principal, quoting the request. Pressing Connect starts the
  ceremony, sends the instructions as a plain message, and answers the
  callback; approval through the provider simulator writes the credential
  as before. `/settings` then shows the account suffix, expiry, and a
  Disconnect button; pressing it removes the credential and the row, edits
  the message, and answers the callback.
- A callback from an unauthorized user, from a non-private chat, or with an
  unknown action is ignored or re-rendered as decided above; every accepted
  callback is answered exactly once; the cursor advances past callback
  updates; a restart does not replay them.
- The old commands are no longer registered and receive the help reply.
- The existing ceremony, expiry, denial, and account-uniqueness assertions
  are kept through the button path.
- Owns `internal/telegram/*_test.go`.

## Validation

- All suites pass under `go test ./...` and `go test -race ./...`, with the
  configured formatting, module, vet, build, and plan checks.
- Live: `/settings` shows the connected model; disconnect and reconnect
  through the buttons; confirm the registered command list in Telegram.
- Coordinate with the operator's GoLand process; do not start a second
  Telegram poller.

## Implementation results

Recorded on 2026-09-06.

- The poller requests `message` and `callback_query` updates. A button press
  passes the same checks as a message: numeric sender, private chat equal to
  the sender, positive message ID, observed names, and an enabled principal.
  Anything else is ignored with the existing reasons or `malformed_callback`
  and is never answered.
- `/settings` renders a title, the lines of each section, and the privacy
  line, with one keyboard row per section. The model section shows "not
  connected" with a Connect button, or the account suffix and token expiry
  with a Disconnect button. Connect starts the device-code ceremony and sends
  the instructions as a plain message; Disconnect cancels the running turn
  and removes the credential and row. The message is edited in place from
  current state after every press.
- Callback data resolves against the fixed vocabulary `<section>:<action>`.
  Unknown, oversized, or stale data only refreshes the view and is logged as
  `unknown_action` without the data. The press is answered before the section
  acts. An edit Telegram reports as unchanged counts as success; an edit it
  refuses falls back to a fresh settings message.
- `/connect`, `/connection`, and `/disconnect` are no longer registered or
  handled and receive the help reply. The not-connected, denied, expired, and
  failed replies point to `/settings`.
- The Telegram client keeps the API error description out of `Error()` so
  the not-modified case is recognised without provider text reaching logs.
- Automated checks passed: formatting, `go mod tidy -diff`, `go test ./...`,
  `go test -race` for the Telegram package, vet, build, `scripts/check-plans`,
  and whitespace. The suites drive the real service through the loopback
  Telegram simulator, which now records answers, edits, and keyboards.

### Live verification

Not performed before this record: the operator reviewed the automated
results on 2026-09-06 and accepted the plan, and the live check of
`/settings`, the Connect and Disconnect buttons, and the registered command
list is the first application smoke test of
[0008](0008-nas-deployment.md).

## Risks and open questions

- Telegram limits edits to messages younger than 48 hours; older settings
  messages fall back to a new message.
- A callback query expires if not answered quickly; the poller answers
  before slower work such as starting a ceremony completes.
- Buttons on a settings message rendered before a restart still work
  because the action vocabulary is fixed and the view is re-rendered from
  current state.
