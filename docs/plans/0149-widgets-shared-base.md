# 0149 — One Widgets switch and a shared base for widgets

Status: `done`

## Context

Plans 0140 and 0142 send a proposal as a Telegram widget for a user who chose it. More widgets are
wanted: a choice picker, lists with actions, a reminder's time, per-item approvals, a weather card
and small charts. Each gets its own plan. Today everything about widgets is written for proposals:

- The per-user switch is "Proposals as a widget", stored as `proposal-widget`, defined in the
  approval package, and shown on a settings page called Confirmations.
- There is one embedded template, `widget.html`, which holds the theme styles, the reading of the
  message's data, the tap-and-wait handling, and the proposal's own view, all in one file.
- The client's widget calls take a proposal (`approval.Shown`) and proposal actions.
- The tap handler authorizes the user and then knows only approval taps.

A second widget would have to copy the template whole and add a branch to each of these.

## Goal

A user has one Widgets switch, and the code has one place for what every widget shares, so the
next widget adds a view, its data and its tap handler and nothing else. Proposal widgets behave
exactly as they do now.

## Decisions

- **One switch, called Widgets, for every widget.** It is stored as `widgets` and is off by
  default. Its definition moves to the integration package as `integration.WidgetsSwitch`: the
  wiring package uses it and must not import the Telegram package, whose tests import the
  wiring. The approval service keeps reading it through the `WidgetPreference` interface it
  already has. The log line `approval.widget_choice.unread` keeps its name.
- **A migration carries each user's choice over.** It renames `proposal-widget` rows to
  `widgets` keeping when they were changed, skips a principal who already has a `widgets` row,
  and then deletes what is left of `proposal-widget`, so it cannot fail on the table's key.
- **An export made before this change restores the choice under its new name.** A restored
  `proposal-widget` row is read as `widgets`, and is skipped when the user already chose
  `widgets`.
- **The settings page is called Widgets.** The Confirmations group, its `nav:confirmations`
  button, the `confirmations` help topic and the `approval.widget.changed` event become
  `Widgets`, `nav:widgets`, `widgets` and `widgets.changed`. Nothing outside the code reads
  the old names. A press of the old button on a settings message already in a chat shows the
  settings overview, as any unknown press does. The section's prefix stays `widget`, so the
  on and off buttons already in chats keep working.
- **What the user reads.** The overview says `Widgets: on` or `Widgets: off`. The buttons are
  "Turn on widgets" and "Turn off widgets". The on line: "Widgets: on. Proposals are shown as a
  widget, a small view Telegram draws in the chat, with the proposal, Confirm, Reject and
  Preview inside it and a one-line caption. A proposal too long for a widget keeps its
  buttons." The off line: "Widgets: off (the default). Proposals are shown with Confirm, Reject
  and Preview buttons. A widget is a small view Telegram draws in the chat; an app that cannot
  draw one shows only its caption." The privacy line: "Widgets: what they show is drawn by a
  small program Telegram runs in the chat, which reaches nothing outside Telegram." A later
  plan adds its widget to the on and off lines.
- **A widget kind is a name and a template; the template is a shared base plus the kind's
  view.** The base holds the theme styles, the reading of the message's data, redrawing when
  the data changes, and one request helper: disable the controls, show "Working on it…",
  ignore an answer that arrives after a redraw, and word a failure. Each request says what
  follows its answer: the controls come back, as after Preview, or the widget waits for its
  data to change and redraws after 20 seconds if it does not, as after Confirm. A view holds
  its own markup, styles and drawing. The files are an embedded directory in the Telegram
  package, and the kinds are a plain table from name to composed template, built once when
  the program starts.
- **The composed template of every kind is committed as a golden file and checked against
  Telegram's limits**, 32768 bytes and 4096 bytes compressed, in the test that checks the one
  template today. A change to what a widget runs is then a diff a reviewer reads, and a view
  that grows too large fails a test and not a send.
- **The client's send call takes a kind, data and a caption line; an edit takes the data and
  the caption line, because a message keeps its kind.** What is particular to a proposal stays
  with the proposal: that it is a widget only when its text fits whole, the shortening of an
  outcome, and the full text sent once as a reply. The generic send has no way to pass an
  inline keyboard, which makes some apps show a widget message as unsupported.
- **A tap is validated and authorized once, then offered to each kind's handler in turn.** A
  handler receives the tap as the existing `press` (chat, message, how to answer), the
  principal and the tap's data, and reports whether the tap was its own, with its result. The
  handler that takes a tap answers it exactly once, within Telegram's 15 seconds, and ties it
  to its own stored record and message, as the approval handler does with the proposal's
  place; a foreign or misplaced proposal tap keeps its own reason in the log. A tap no handler
  takes is answered as doing nothing.
- **No shared table of widget messages now.** A tap names no tab and carries no message text,
  so a kind without a record of its own has neither; the first plan with such a kind, likely
  the reminder's time or a list action that replies, adds the table.
- **A proposal widget's data keeps its shape** (`text`, `status`, `actions`), and the approval
  handler stays for as long as a proposal already in a chat can be tapped. A widget message
  keeps the template it was sent with.

## Approach

One worker implements it in two stages, with the test suite passing after each (Jev tier:
complex, 0.98; the split of the template is judgment work on something a sent message keeps).
An independent review follows.

Stage 1, the switch:

1. Migration `056` as decided above.
2. `integration.WidgetsSwitch` replaces `approval.WidgetSwitch`; wiring passes its toggle to
   the approval service and registers its settings as now.
3. The settings group, page texts, help topic, privacy line and event are renamed.
4. The export's switches section maps the old name on restore.

Stage 2, the base:

5. `widget.html` becomes the base files and the proposal view under
   `internal/telegram/widgets/`, with the table of kinds and the golden file.
6. The client's widget send call becomes generic over a kind, and the edit call over the
   data; the proposal's fit, shortening and reply rules move to a proposal file that uses
   them.
7. `processWidgetQuery` becomes the shared part and a list of handlers, with the approval
   handler as the only one.
8. `docs/architecture.md` describes the switch and the base.

## Files affected

- `internal/store/sqlite/migrations/056_widgets_switch.sql`
- `internal/integration/switch.go`, `internal/approval/approval.go`,
  `internal/wiring/wiring.go`, `internal/wiring/export.go`
- `internal/telegram/switch_pages.go`, `settings.go`
- `internal/telegram/widget.go`, `widget.html` (replaced by files under `widgets/`), a new
  proposal-widget file, `client.go`, `service.go`
- Tests: `internal/telegram/widget_integration_test.go`, the settings tests that list the
  overview's buttons, a store test for the migration, a restore test for the export
- `docs/architecture.md`, `docs/plans/README.md`

## Validation

- The existing widget tests pass, changed only where they name the switch, the page and its
  texts.
- Migration: a principal with `proposal-widget` on has `widgets` on, one with it off has it
  off, one who never chose has no row, a principal with both rows keeps the `widgets` one, and
  no `proposal-widget` row remains.
- Restore: an archive carrying `proposal-widget` turns `widgets` on, and does not change a
  `widgets` choice the user already made.
- Settings: an old `nav:confirmations` press shows the overview; the help tool offers
  `widgets` and not `confirmations`; `/privacy` shows the widgets line while the switch is on.
- The golden-file and limits test covers every kind.
- A tap whose data no handler takes is answered as doing nothing and logged as ignored.
- Formatting, vet, build, module tidiness, the full test suite, the race run on the Telegram,
  store, approval and wiring packages, and `scripts/check-plans`.
- Before deploy: the composed proposal template is sent to the operator's chat with sample
  data by a send-only script, with no polling, and looked at in a phone app and a desktop app.
  Preview is tapped once: with nothing polling it shows "Working on it…", then that no answer
  came, and the controls come back, which proves the request helper runs.
- After deploy, with a database backup taken first: `/settings` shows Widgets on for the user
  who had it on; on a short proposal Preview answers and the controls come back, Reject shows
  its outcome, and on another Confirm does; a proposal widget sent before the deploy still
  shows its outcome when resolved.

## Risks and open questions

- The widget's script has no automated test. The golden file makes a change to it visible,
  not proven; the two live checks are the proof. A broken script would leave proposals sent
  with it unconfirmable until they expire, and turning the switch off does not repair a
  message already sent.
- The previous image does not start on a database with this migration applied. Going back
  means restoring the backup taken before the deploy, or fixing forward.
- The base's request helper is shaped by one view and the two ways a request ends that it
  has. Messages already sent keep the base they were sent with, so data shapes must stay
  readable by older views.
