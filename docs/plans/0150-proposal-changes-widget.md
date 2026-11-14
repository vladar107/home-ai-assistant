# 0150 — Choosing the changes of a proposal in a widget

Status: `done`

## Context

With Widgets on, a proposal is a widget only when its whole text fits the 1024 bytes of data a
widget message holds. A proposal of several short changes fits and is shown whole in the
proposal widget. One of several longer changes, such as two backlog items with details, does
not fit and is sent as text with Confirm, Reject and Preview buttons, as it is with Widgets off.

Confirm and Reject apply to every change of a proposal. Plan 0140 left choosing among them to a
later plan. The approval service can already end single changes on their own: a change the
assistant withdraws ends rejected while the others wait, and the message shows what became of
each.

The approval service refuses a proposal it cannot show whole, because the user must see the
exact action before confirming it. Plan 0149 gave widgets a shared base, a table of kinds, and
one tap handler per kind.

## Goal

With Widgets on, a proposal of several changes is a widget that lists them. The user reads any
change in full inside the widget, leaves out the ones not wanted, and confirms the rest or
rejects them all. A proposal of one change, and every proposal with Widgets off, behaves as now.

## Decisions

- **A second kind of widget, `changes`, lists a proposal of several changes.** With Widgets on,
  every proposal of two or more changes is a `changes` widget, with all its changes in that one
  message and no text messages before it. A proposal of one change is the proposal widget when
  it fits and text with buttons when it does not, as now. The approval service offers the whole
  proposal to the deliverer through the call it uses today, with the proposal's changes in what
  it shows; when no `changes` widget is sent, because the data cannot fit, Telegram refuses it,
  or part of the proposal already reached the chat as text, the proposal goes out as text with
  buttons, with no proposal widget tried.
- **Confirming from titles is an accepted relaxation of "show the exact action".** The owner
  chose it over keeping the full text on screen. Each change's full text is one tap away, and
  three rules keep a title meaningful. A proposal may carry a title of its own, which a
  connector sets when its summary starts with something other than the change: the weekly
  review puts where a card came from, and any warning, above the change, so it sets the title
  to the change's own first line. Without one, a title is the first line of the summary after
  any warning, joined with the next line when the first ends with a colon. A change whose
  summary starts with a warning, the intent check's or a connector's line starting with ⚠️,
  shows the warning's mark before its title and the warning's sentence under the list. And a
  proposal whose titles cannot each keep 24 characters, or the whole title when it is shorter,
  is not a widget. The title is stored with the record.
- **What the list shows.** A heading: the heading of an offered proposal if it has one, and the
  number of changes. Each change on a line: its number, a tick, and its title. Under the list,
  where the proposal stands: its expiry while it waits, and how many changes ended which way
  once any is resolved, each change showing its own outcome line. The controls are "Confirm N of
  M", "Reject all", and Preview where the buttons have it.
- **A change is read in full with a tap on its title.** When every change's full text fits the
  data beside the titles, the texts travel in the data and a tap opens one with no request.
  Otherwise the widget asks the bot for that change and shows its summary under its title; a
  second tap folds it. The bot answers with the summary as plain text, for a change of the
  user's own proposal whatever became of it. A summary longer than an answer holds is answered
  by its beginning and, after the tap is answered, sent whole once as a reply to the widget, as
  a preview's output is. The Telegram layer decides that it does not fit and remembers by record,
  in memory pruned as presses are, after the reply is sent, that it was sent; after a restart it
  may be sent once more.
- **The choice is made in the widget and sent with Confirm.** Every change starts ticked. A tick
  costs no request. Confirm names, as one flag per change, which are kept, by position in the
  proposal as it was shown, and is disabled while none is kept. Positions do not change: nothing
  joins a proposal once it is offered to the chat, and a withdrawn change keeps its number.
  Ticks and opened changes survive a redraw while the widget stays loaded, and start again as all
  ticked and folded when it is loaded anew.
- **A change left out ends rejected, with the reason that it was left out.** One store call, in
  one transaction, checks again that a kept change is still pending and unexpired, and only then
  ends the pending changes left out and confirms the kept ones; it returns both, and changes
  nothing when no kept change waits. The kept ones then run in their order, as now. The message
  says "Left out; nothing was written." for such a change, the caption and the tally count it as
  left out, and the assistant is told which of its proposals the user left out, apart from the
  ones the user rejected. Its status stays `rejected`, with the reason `left_out`, so the weekly
  review and the dashboard's rejection rate count it as they count a rejection.
- **A Confirm that names changes is checked before the tap is answered.** It must name as many
  changes as the proposal shows and keep at least one that is still waiting; a change no longer
  waiting is passed over whatever its flag says. A Confirm that fails the check, before the
  answer or inside the store call, is answered "This proposal changed; look at it again." when it
  can still be, and resolves nothing. Every Confirm tap, taken or not, ends with an edit of the
  message to the proposal's stored state, with its controls while anything still waits, as a
  withdrawal's edit keeps them, so a retried tap heals a failed edit. The choice and the reading
  of a change come from widget taps only, from the message the proposal is shown in. Callback
  data that starts with `approval:` and is not a button's is answered and ignored, as an unknown
  `undo:` press is, and the parser for widget taps is apart from the buttons' parser.
- **The record keeps which view its message runs.** An edit must send the data that view reads,
  and both views stay in use. The record's widget flag becomes the widget's kind: `proposal`,
  `changes`, or none for a text message. Records flagged today become `proposal`. Where the
  code learns only that a message is a widget, from Telegram refusing a text edit or from a tap,
  it takes the kind as `proposal`: a `changes` message is always recorded with its kind, because
  a record that failed to save its message cannot be tapped.
- **The data of a `changes` widget** uses one-letter keys and holds the proposal's ID, the
  heading, each change's title, state, warning mark and, once resolved, its outcome line, the
  full texts when they fit, the status, and whether Preview is offered. State words and the
  warning's sentence are in the template, not the data; a state the view does not know is drawn
  by its outcome line, so a template already sent never goes blank. The view builds what its
  taps send from the ID. Titles are shortened alike until the data fits, down to the floor
  above. Once changes are resolved, titles keep that floor and outcomes are shortened below it;
  when an outcome had to be shortened, the message's text is sent once as a reply. Shortening
  titles alone sends no reply. A widget already sent keeps its template, so the data's shape
  only grows.
- **The text of a `changes` message is its numbered titles with their outcome lines**, not the
  full summaries. It is what the reply after resolution carries, and what the reply carries when
  Telegram refuses an edit for good; both are split into as many messages as they need, as a
  preview's output is.
- **One long change keeps its buttons.** Its text overflows an answer as surely as the data, so a
  list of one would almost always end in a reply carrying the text anyway.
- **The base's request helper gains a third ending:** the answer is handed to the view, which the
  reading of a change needs. The proposal view is composed from the changed base, so its golden
  file changes; a message already sent keeps the template it was sent with.
- **What the user reads in `/settings`.** The on line's last sentence, "A proposal too long for
  a widget keeps its buttons.", becomes "A proposal of several changes is a list: tap a change to
  read it, untick the ones to leave out, and confirm the rest. A single change too long for a
  widget keeps its buttons." The off line and the privacy line stay.
- **The caption stays one line**, the number of changes and, once resolved, what became of
  them. An app that draws no widget shows only that, as with the proposal view.

## Approach

One worker implements it in three stages, with the full test suite passing after each. The
approval service's new behaviour is tested by calling it in the Telegram package's harness, as
offered proposals are, and against a real store in the approval package where a test needs to
step between two store calls. An independent review follows.

Stage 1, the widget's kind (Jev tier: complex, 0.70):

1. Migration `057` replaces the record's widget flag with its kind, a text column with no list
   of allowed values, so a later kind needs no table rebuild.
2. `Record.Widget` and `Shown.Widget` become the kind; the deliverer reports the kind it sent.
   This touches the Telegram client and service and the approval test doubles in the
   connectors, wiring and trip packages, and the tests that read the old column. No behaviour
   changes.

Stage 2, choosing in the approval service (Jev tier: complex, 1.00):

3. The store call that checks, leaves out and confirms in one transaction; the left-out reason,
   its status line, tally, caption word and notice.
4. What a proposal shows gains its changes, each with a title, state, warning mark and outcome,
   and the ID its taps name; the text of one change for the principal who owns it. A proposal's
   own title, set by the weekly review, stored in a column the same migration `057` adds.
5. The whole proposal of several changes is offered through the existing `SendProposal`, its
   changes in what it shows, so the deliverer's interface and its test doubles do not change; the
   client sends it as today until stage 3.

Stage 3, the Telegram side (Jev tier: complex, 0.96):

6. The base's third ending; the `changes` view, its golden file and the limits test.
7. Encoding the view's data with the shortening above; sending and editing by the record's
   kind; the split reply after resolution.
8. The tap data for a choice (`approval:keep:<id>:<flags>`) and for reading a change
   (`approval:read:<id>:<number>`), and the approval handler taking both, with the reply-once
   memory for a long change; ignoring a button press with `approval:` data that is not a
   button's; the split reply after a refused edit.
9. The settings text; `docs/architecture.md`.

## Files affected

- `internal/store/sqlite/migrations/057_approval_widget_kind.sql`, `internal/store/sqlite`
  (approvals and their tests)
- `internal/memory` (the weekly review sets each card's title)
- `internal/approval` (service, withdrawal, preview's reply path as the pattern for a long
  change)
- `internal/telegram`: `widgets/base.html`, new `widgets/changes.*`, `testdata/widgets/`,
  `widget.go`, `proposal_widget.go`, a new file for the `changes` data, `client.go`,
  `service.go`, `switch_pages.go`
- Approval test doubles in `internal/connectors/homeassistant`, `internal/wiring` and
  `internal/trip`
- Tests: `internal/telegram/widget_integration_test.go`, a store test for the migration
- `docs/architecture.md`, `docs/plans/README.md`

## Validation

- Widgets on, two changes that fit whole: one `changes` widget carrying their texts, which open
  with no request. Two changes that do not: one `changes` widget with titles only, and no text
  message. The caption counts them.
- One change: the proposal widget when it fits, and text with buttons when it does not.
  Widgets off: the messages and buttons of today.
- Titles: a first line ending in a colon is joined with the next; an intent-warned change shows
  the mark, the sentence under the list, and the warned caption; a proposal whose titles would
  drop below 24 characters is sent as today.
- Reading a change: the answer is its summary; a long one is answered by its beginning and
  replied in full once, and a second tap sends no second reply. A change of another principal's
  proposal, or a tap from another message, is answered as doing nothing.
- Confirm keeping the first of two: the first runs, the second ends left out, the widget shows
  both, the caption counts both, the assistant's notice names the one left out, and the turn
  that continues the request is queued. A replayed Confirm acts once and the message shows the
  outcome.
- Confirm naming no changes, from a button, confirms all; Reject all rejects all. A button press
  with a choice in its data is ignored.
- A Confirm naming the wrong number of changes, or keeping none that wait, is answered as
  changed, resolves nothing, and edits the message to its stored state. A kept change the
  assistant withdrew meanwhile is passed over and the rest run. A Confirm after the deadline
  expires all and leaves none out.
- An offered proposal with a heading: no continuation, and no notice.
- Expiry, a restart, and a withdrawal while pending edit the `changes` widget in its own shape;
  after a withdrawal the controls stay.
- A resend after a failed delivery records the kind of what was finally sent; a proposal partly
  delivered as text stays text.
- Ten changes with titles in a script of two-byte letters fit the data, before and after
  resolution; the longest tap the view builds is at most 64 bytes, which the fake Telegram
  checks.
- A `changes` widget Telegram refuses: today's messages with buttons. Widgets turned off while a
  list is pending: the list still works. A tap in a tab continues in that tab.
- A record of the `proposal` kind with two changes, seeded as a message sent before this change,
  is edited in the proposal view's shape, with its shortening and reply; the existing tests of
  proposal widgets with several changes move to the `changes` view.
- A `changes` widget whose outcome edit Telegram refuses for good: the outcome arrives as a
  reply split into messages that each fit, as for a proposal widget today.
- Preview in a `changes` widget previews the whole proposal and gives the controls back.
- A Confirm tap whose flags hold anything but 0 and 1, which no view builds, is answered as
  doing nothing. Reading a change already resolved answers its summary.
- A Confirm whose kept changes are all withdrawn between the check and the store call leaves
  every change as it was.
- Migration: a record flagged as a widget has the `proposal` kind, and one not flagged has none.
- A weekly review's cards are titled by their own change, a flagged card carries the warning
  mark, and two cards with the same reason have different titles.
- The golden-file and limits test covers both kinds.
- Formatting, vet, build, module tidiness, the full test suite with and without the race
  detector, and `scripts/check-plans`.
- After deploy, with a database backup taken first: a proposal of two long changes arrives as a
  list; a change opens and folds; confirming one of two runs it and shows the other left out;
  another proposal rejects whole; a short single proposal still arrives in the proposal view,
  where Preview answers and gives the controls back and Confirm shows its outcome.

## Risks and open questions

- A user can confirm a change whose full text they never opened. The intent warning, its mark,
  and the title rules above are what stand in for the full text.
- How tall a widget may grow, and whether one scrolls, is not known. A long change opened in the
  list may be cut off by the app; the reply sent for a long change is what covers it.
- Ticks are lost when the widget is loaded anew, and every change is kept again. A user who
  unticked, left the chat and came back confirms everything unless they look. The Confirm label
  says how many are kept.
- The read tap's format and the data's shape are permanent for messages already sent.
- The poller handles one update at a time, so a tap that waits behind a long Confirm can miss
  Telegram's 15 seconds and show the user that no answer came.
- The view's script has no automated test, as with the proposal view. The golden file makes a
  change to it visible and the live checks prove it.
- The previous image does not start on a database with this migration applied. Going back means
  restoring the backup taken before the deploy, or fixing forward.
- Preview stays for the whole proposal; previewing one change is not part of this plan.
- The buttons' Confirm keeps its own path; the race in it between a withdrawal and a press
  predates this plan and is left for another.
