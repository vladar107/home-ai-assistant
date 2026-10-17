# 0093 — Vacuum schedule through Home Assistant

Status: `done`

## Context

Since 2026-10-10 the robot vacuum's in-app schedule has been reachable only
through a Home Assistant script exposed to Assist. The script wraps the
vacuum integration's set-property service for the schedule, and Home
Assistant's MCP server offers it to the model as `ha_vacuum_schedule`. The
script takes three fields: `current_time`, `enabled`, and `new_time`. A
markdown card on a Home Assistant dashboard shows each task's time, days,
and rooms.

A session on 2026-10-17 showed three problems:

- **Days, cleaning type, and rooms cannot be changed.** The script has no
  field for them, so the model kept retrying with what it had.
- **Reading needs a confirmation.** `mapTool` treats every tool whose intent
  does not start with `Get` as an action, so a call with no arguments, which
  only reads, is a write. The conversation had quoted a report that read
  outside content, so every call became an `ha_action` proposal. That was
  nine proposals in four minutes, four of them in one bundle, and most had
  `Arguments: {}`. The user had to confirm a read to see the schedule, and
  each confirmation's continuation proposed again.
- **The output is JSON.** The proposal shows the arguments as JSON, and
  `executeApprovedAction` sends the script's response object to Telegram
  as-is.

The vacuum integration parses the schedule but does not decode it, and its
maintainers declined schedule writing. The format was calibrated on the
household's robot on 2026-10-17: a test task was saved in the vendor app
with one setting changed at a time, and the vacuum's state attribute was
diffed after each save. The decoded layout is specific to the device, so it
stays with the Home Assistant files in the operator's local folder (see
Decisions). The bot needs none of it.

## Goal

The user can ask "what's the vacuum schedule?", "move Tuesday's clean to
Wednesday and Friday", "make the Saturday clean mop after vacuuming", or
"have the 11:10 clean do only the hallway and the kitchen, with the hallway
on max".

- Reads answer in plain sentences, with weekdays, rooms, and settings. They
  never ask for confirmation.
- A change runs at once in a clean conversation. After outside content it
  is proposed with readable fields, and it reports in a sentence what
  changed.
- The dashboard card shows the same detail: mode, suction, route, passes,
  moisture, and each room's settings.
- No JSON reaches the user.

## Decisions

- **Home Assistant stays the only point of contact for the vacuum.**
  - The bot reaches the schedule only through scripts the household exposes
    to Assist, never by calling the vacuum integration's services itself.
  - The bot's changes are general Home Assistant improvements, with nothing
    specific to the vacuum.
- **One shared decoder in Home Assistant.**
  - A custom template file under `custom_templates/` holds the macros. They take the entity id as an argument.
    - `decode` returns the tasks as JSON: time, days, enabled, scope, mode,
      suction, route, passes, wetness or moisture, and rooms by name with
      their own settings.
    - `describe` returns the same tasks as plain sentences.
    - `encode` applies an edit and returns the new schedule string.
  - Both scripts and the dashboard card import the file, so all three always
    agree on what the schedule means.
- **Two scripts.**
  - `script.vacuum_schedule` keeps its name and becomes read-only, with no
    fields. It returns `{"summary": <describe>}`.
  - A new `script.vacuum_schedule_change` takes the following fields:
    - `task`: a start time, HH:MM
    - `days`, `time`, `enabled`
    - `rooms`: room names, or `all`
    - `mode`, `suction`, `route`, `passes`, `moisture`
    - `room`: apply the settings to one room only
    - `dry_run`: return the new string without writing it
  - The change script returns `{"summary": "<task>: <before> → <after>"}`.
    It stops with a plain sentence on any bad input: an unknown room, a
    missing task, mop after vacuum for rooms, or a value out of range.
  - **Switching between whole map and rooms.**
    - Choosing rooms on a whole-map task gives each room the task's
      settings, with wetness as moisture. Mop after vacuum is refused
      unless a mode is given.
    - Going back to the whole map takes the rooms' settings when they
      agree, and otherwise needs them given.
  - Unknown bits are copied from the entry being edited, and only fields
    the call names change.
- **A read-only label, `assistant-read`, in the bot.**
  - A script entity that carries this Home Assistant label maps to a read
    tool, so it never needs confirmation and scheduled runs may use it.
  - The label is read from the registries `labelledEntities` already
    fetches. It matches the label's name only: it grants rather than
    restricts, and a renamed label keeps its id.
  - An unlabelled script stays an action. A script labelled both
    `assistant-read` and `assistant-confirm` stays confirmed, judged from the
    registry alone.
  - The upgrade happens only when both the labels and the live context were
    read. Any failure leaves every script a write.
  - The household decides in Home Assistant which scripts only read.
- **Readable script results in the bot.**
  - When a result is an object with exactly one string field,
    `unwrapResult` returns that string. This applies to direct calls and to
    `executeApprovedAction`.
  - Proposals show their arguments as `field: value` lines instead of JSON.
- **Where the Home Assistant files live.** The template, scripts, and card
  name household entities, so they stay out of this public repository. They
  live in the operator's local Home Assistant folder, next to the earlier
  inspection helpers. The repository carries only the general bot changes.

## Approach

1. **Calibration (orchestrator, with the user).** Done; see Context.
2. **Home Assistant templates (orchestrator).**
   - Write the custom template, the two scripts, and the card. Base them on
     the 2026-10-10 script and card, and fix the scope flag those read as a
     map id.
   - Test them locally with jinja2 and stand-ins for Home Assistant's
     `bitwise_and`, `bitwise_or`, `to_json`, and `from_json` filters,
     against every captured state:
     - decoding and then encoding reproduces each capture's tasks exactly
     - `describe` reads correctly for every capture
     - every edit kind changes only its own bits
   - Run `dry_run` calls against the live Home Assistant with the household
     token before anything is written.
3. **Bot changes (complex, Sonnet; the orchestrator reviews the label and
   approval wiring).** In `snapshot.go`, `tools.go`, and `action.go`, with
   integration tests in `service_test.go` against the fake Home Assistant
   server:
   - a script labelled `assistant-read` is offered as a read, and creates
     no proposal in an exposed conversation
   - an unlabelled script is still proposed there
   - a script with both labels stays confirmed
   - a one-string result object comes back as plain text, both directly and
     after approval
   - a proposal summary contains no `{`
4. **Rollout (orchestrator, with the user).**
   - Run the configured checks, tag, and deploy the bot.
   - Take a fresh backup of the schedule.
   - The user places the custom template on the NAS, reloads custom
     templates, and pastes the scripts and the card.
   - The user labels `script.vacuum_schedule` `assistant-read`, and exposes
     `script.vacuum_schedule_change`.
   - Live read through the bot, then one live change on the test task,
     confirmed in the vendor app.
   - The user deletes the test task in the app.
   - Update the operator memory note on the vacuum schedule.

## Files affected

- `internal/connectors/homeassistant/snapshot.go`, `tools.go`, `action.go`,
  `service_test.go`
- `docs/plans/0093-vacuum-schedule-tools.md`, `docs/plans/README.md`
- Outside the repository, in Home Assistant:
  - the custom schedule template
  - `script.vacuum_schedule` and `script.vacuum_schedule_change`
  - the schedule card

## Validation

- Formatting, module, test, vet, build, and plan checks.
- A local template round-trip of every capture, and live `dry_run` calls.
- A live read through the bot in an exposed conversation, with no
  proposal.
- One live change, confirmed in the vendor app and on the card.

## Risks and open questions

- The template encoding cannot be covered by the repository's tests. The
  local harness and `dry_run` calls stand in for them, and the harness
  stays in the operator's folder.
- The state attribute is the integration's parse of the raw string, so it
  hides whether a task was enabled with `1` or `2`. The scripts write `1`.
- Custom templates need a file on the Home Assistant host and a reload by
  an admin, so the user does that step.
- Firmware updates may move bits. `decode` reports an entry it cannot map
  as unknown rather than guessing, and `encode` refuses to edit it.
