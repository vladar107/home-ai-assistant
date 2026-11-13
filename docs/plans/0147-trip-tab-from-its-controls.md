# 0147 — A trip moves to the user's tab from its own controls

Status: `proposed`

## Context

Plan 0141 lets a trip take a tab the user already has, when the user says so in that tab and the
turn passes `use_this_tab` to a trip tool. On the deployed bot the user said it and the turn
answered without calling the tool. Nothing but the model's reading of that sentence can move a
trip, so one missed reading left the trip in the assistant's tab; when the user then deleted that
tab, the assistant made another.

The tool hint that nudges a turn towards one tool picks from short tool descriptions, and the
trip tool's description mentions the tab last.

## Goal

The user can make a tab the trip's tab without depending on how a sentence is read, and saying it
is recognised more reliably.

Success: opening the settings in a tab of their own and pressing the trip's button moves the trip
there with everything it sends, with the refusals plan 0141 gives.

## Decisions

- **The Trips section of the settings offers the move.** For each planned trip that has not ended,
  it shows a button "Use this tab for <title>" when the tab the settings were opened in can be the
  trip's, by the check the tool uses. Pressing it takes the tab as the tool does and answers with
  the result. Where the tab cannot be a trip's, such as the main view or a feed's tab, the section
  says in one line to open the settings in the tab the trip should use.
- **The section shows trips whether or not the scan is on** and whether or not mail can be read;
  the scan's own switch stays as it is.
- **At most five trips get a button,** nearest first, and a button names the trip by its ID, which
  fits the callback limit.
- **The settings opened as the first message of a new tab still belong to the main view,** as any
  command does, so the user opens them in a tab that already exists; the section's line says so.
- **The trip tools lead with the tab.** The descriptions of `trip_update` and `trip_add` start
  their tab sentence, and the tool hint gets an example of the request, so "this is the tab for
  the trip" points at the tool.
- **A deleted trip tab is replaced as today.** Asking first was considered: a tab taken for gone
  leaves no record, so the trip would need a marker of its own. The button makes that unnecessary.

## Approach

1. The section: a trip reader and the tab check in its page, the buttons, the callback that takes
   the tab, and the line for a tab that cannot be used.
2. The tool descriptions and the hint example.
3. Tests through the Telegram simulator: the button appears in a tab the user named and moves the
   trip, its summary, and a later notice there; it is absent with the line in the main view and in
   a feed's tab; a press after the tab stopped being usable is refused with the reason; a sixth
   trip has no button.
4. Documentation: the in-bot trips text and `README.md`.

## Files affected

- `internal/telegram` (trips section, its wiring to the trip reader and the tabs), tests
- `internal/trip/tools.go`, `internal/judgment` (the hint example)
- `internal/wiring`
- `README.md`

## Validation

- Formatting, module, vet, build, race and non-race tests, and `scripts/check-plans`.
- The tests named in the approach.
- Live, by the operator after deployment: move a planned trip from the settings in a tab.

## Risks and open questions

- **The model may still miss the spoken request;** the button is the dependable way and the
  descriptions only raise the odds.
- **This plan lands after plan 0144,** which changes the same tab code.
