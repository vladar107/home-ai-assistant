# 0119 — A Heads-up that plans the day well

Status: `done`

## Context

A review of the operator's chats from 16 to 28 October found Heads-up
(plan 0081) unreliable:

- All 26 runs sent a message. In practice it is a daily briefing, mostly about
  travel.
- It was wrong about the commute on most days:
  - One morning it checked only the way home and skipped the trip to the
    office, although the office day was marked in the calendar and a saved
    route fit it.
  - It chose a "leave at 16:00" route home although an event at the same place
    ran until 18:30.
  - An evening run treated a meetup in another city as an office day. It also
    reported a departure for the way home that had already left.
  - One morning it included the next two days and missed both the way home and
    that morning's disruption.
- When the saved train was partly cancelled, it said to check before leaving
  and offered no alternative. `route_check` puts nothing in place of a failed
  departure, and the run never called `ns_trips`. When the user asked a few
  minutes later, the first `ns_trips` call found a train.
- A saved route lists two departures that NS never runs. Every report said
  so again.
- Non-travel items were repeated across days: a device wear level appeared in
  6 reports and a health trend in almost all of them. `job_reports` gives
  excerpts only, and the prompt asks to avoid repeating only today's reports.
- Each complaint led the chat assistant to rewrite the job's prompt: four
  rewrites in three days. The prompt is now 1000 characters of terse,
  user-specific rules, such as a weekday exit rule with an exception. The
  model misreads them, and every patch causes the next miss. The built-in
  prompt is copied into the job when it is created, so improvements in code
  never reach an existing job.
- Scheduled turns run on the chat default (`gpt-6-luna:medium`). Each run did
  one round of parallel lookups, at most one follow-up, and then wrote. It
  used about 12 of its 20 tool calls.
- The weather check failed for a calendar location given as a street address.
  Geocoding matches only the text before the first comma.

The operator does not want commute logic hardcoded. The fix is a stronger
model, a better prompt, and tools that return more.

## Goal

Heads-up covers every trip in its window correctly, from the calendar and
saved routes, including the first trip from home and the last one back. When a
saved train fails, it gives a concrete alternative. It drops anything already
past, and repeats a non-travel item only when it has changed. The user's
corrections become short preferences that persist, never rewrites of how the
briefing works.

## Decisions

- **A stronger model for each model-turn job, which the user can change.**
  - A model-turn job's config gains an optional `model` (`id:effort`).
  - Chat sets it through `schedule_propose` and `schedule_update`, checked
    against the principal's catalog the way `/model` checks a choice. With an
    unreadable catalog, the chat, task, and Heads-up defaults are accepted. It
    passes through the confirmed job and update payloads, so Confirm,
    preview, and Undo carry it.
    `schedule_get` shows the model the job runs on and where that choice
    comes from.
  - At run time the job's own choice wins. The Heads-up job without one uses
    the new setting `HOME_AGENT_PI_HEADSUP_MODEL` (default
    `gpt-6.1-sol:high`). Every other job uses the chat default, as today.
  - A choice the catalog or the subscription no longer offers falls back to
    the chat default. The rule in `task.Service.EffectiveModel` (catalog
    support, then the cached usage read) moves into a shared helper that
    tasks and the schedule service both call, rather than being copied. The
    schedule service config gains the catalog and cached-usage readers. The
    fallback is logged.
  - The choice is resolved at run time, not copied when the job is created,
    so changing the setting moves existing Heads-up jobs.
- **Scheduled model turns get their own time limit.** This and the call
  budget below follow from the stronger model rather than from the request.
  - `HOME_AGENT_SCHEDULED_TURN_TIMEOUT` (default 3m, at most 10m) replaces the
    chat turn timeout in `RunScheduled`. Checks, reviews, and chat are
    unchanged.
  - Scheduled runs keep holding the principal's turn lane, so a chat message
    sent during a run waits for it, up to this limit. 3m keeps that wait
    short. A run that overruns delivers the timeout reply as a failure report,
    which is visible, so the limit is checked against the first live runs.
  - Heads-up's call budget rises from 20 to 30, since the method checks every
    trip and the alternatives for each failed train.
- **Heads-up = a built-in method + the user's notes.**
  - The method lives in code and is placed before the stored text on every
    Heads-up run. It is general, with nothing specific to one user:
    1. Set the window: from now to the end of today, plus all of tomorrow
       when the run is after 15:00 local time. A morning run therefore
       covers today only. The operator's own notes already ask for this, and
       the old built-in prompt did not.
    2. Build a timeline of where the user will be, from every calendar event
       in the window, with its place. An all-day event marks a place or state
       for the day, and the notes say how to read the user's calendar.
    3. Treat every move between places as a trip, including the first one
       from home and the last one back after the final event of the day.
    4. Check each trip with a saved route that fits it, or plan it with the
       transit tools: arrive before the next event starts, and leave after
       the previous one ends.
    5. Whenever a check fails or a connection is too tight, find the real
       alternatives and give one with its times. Never tell the user to check
       for themselves.
    6. Check disruptions on those trips, and the weather where the user will
       be outside. When writing, send one `weather_card` for the window, with
       each trip's trains.
    7. Check home devices, health and energy trends, and pending reminders.
       Read the last three days of Heads-up reports by the job's ID, which a
       rename keeps, and repeat a non-travel item only when it changed or
       needs action now.
    8. Check the draft before sending: every trip covered, every event from
       the calendar, every time and status from a tool in this run, nothing
       in the past.
    9. Reply with `[nothing-to-add]` only when nothing in the window needs
       attention.
  - The job's stored text becomes the user's notes, kept to 1000 runes: their
    preferences, which override the method on style, language, and how to read
    their calendar. A new Heads-up job stores short default notes. The text
    of `HeadsUpPrompt` moves into the method. `ModelTurnConfig(prompt)` keeps
    its signature; a second constructor takes the model, so the Heads-up
    switch is untouched until the notes change.
  - `schedule_get`, `schedule_list`, and the job list in the system prompt
    label Heads-up's text as notes and say the method is built in.
  - A Preview of an update to the Heads-up job runs with the method and the
    Heads-up model, since the update carries the job's ID.
  - The schedule guidance tells chat to answer a correction about a Heads-up
    report by adding or editing one note with `prompt_edits`, written as a
    preference, without restating the method. This is the same edit path
    other jobs use.
- **Existing notes.** At start, a Heads-up job whose text is still exactly the
  old built-in prompt gets the new default notes, on whatever schedule, after
  `MigrateHeadsUp` moves the legacy schedule. Its model, schedule, and next
  run are kept. A job whose text the user changed keeps it as its
  notes. The operator's edited notes are rewritten as preferences only, drafted
  from their current text and sent in chat after the deploy.
- **`route_check` offers alternatives.**
  - A new optional interface, `transit.Alternatives`, plans the next trips
    from one place to another (with an optional via) leaving at or after a
    given time.
  - NS implements it with its existing trip planning. Trip planning returns
    trips around the given time, so the implementation keeps only those that
    leave at or after it. When the plain plan's first trip has a
    train-to-train transfer of 2 minutes or less, NS also asks for the plan
    with 10 extra minutes per transfer (plan 0116), the first step of the
    10/20/30 ladder its departure checks use, and returns both. Buses do not
    implement it, so a failed bus leg stays as today.
  - When a train leg has no usable departure, the route check asks for up to
    3 alternatives from when the user can be at the station, or from the
    first saved departure if that time is unknown. The route continues on the
    first alternative that runs, so the leave time, arrival, and target
    verdict still come out.
  - The result marks those legs as alternatives, and states that the saved
    departures failed and why. A departure gains a marker for being an
    alternative, so the leg keeps the alternative's own status (on time,
    delayed) and `take` can still use it.
  - A saved departure that NS never runs is named in the result as one to
    replace, with the nearest real departure, so chat can propose fixing the
    route.
  - The routes service finds the interface on its `Trains` service by type
    assertion, so wiring is unchanged.
  - A retry trip with the same first train as a plain-plan trip is not listed
    on its own. Its later connection becomes a note on that trip ("if the
    change is missed"), so the 3 alternatives are distinct first trains.
  - Route watches keep alerting when the saved trains fail, and also alert on
    a delay or platform change of the alternative taken. A leg counts as having
    no way through only when no alternative runs either.
  - Route watches check through `CheckForWatch`, which asks for alternatives
    only when a leg cannot go on and skips naming the nearest real departure:
    a watch checks every few minutes, and that lookup is for the user reading
    `route_check`.
- **`job_reports` can return full texts.** A new optional `days` argument
  (1–7), always with `job`, returns the full text of that job's reports from
  the last `days` days, newest first. The answer shares one budget of
  `reportResultBytes`, under the broker's 16 KB result limit. The newest
  reports come whole, and the oldest ones that do not fit are cut and marked
  as cut. Without `days`, the listing is unchanged.
- **Weather accepts addresses.** When the text before the first comma finds
  no place, geocoding tries each later part of the address in order. It
  first strips leading tokens that contain a digit (a postcode in most
  formats), so `Street 1, 1234 AB Town, Country` resolves to the town. A place name works as before.

## Approach

Waves of independent chunks. Within a wave, chunks touch disjoint files.

**Wave 1**

1. **Model per job and time limit for scheduled turns.**
   - New settings parsed in `internal/config`.
   - The shared model fallback helper, used by `task.Service`.
   - `RunScheduled` takes a `ModelEffort` and the time limit. Fake
     `RunScheduled` implementations in tests follow the signature.
   - The `model` field on the model-turn config, resolved in `modelturn.go`
     with the fallback.
   - The `model` argument on `schedule_create` and `schedule_update`,
     validated against the catalog through a narrow interface in the
     schedule service config. `schedule_get` shows the model.
   - Wiring, and integration tests.
2. **Alternatives in `route_check`.**
   - The `transit.Alternatives` interface.
   - The NS implementation, on top of `planTrips`.
   - Alternatives in `routes.Service.Check`, and their rendering in the
     route tools.
   - Integration tests against the fake NS server.
3. **Weather by address.** Address fallback in `geocode`, with a test against
   the fake geocoder.

**Wave 2** (after chunk 1, which also edits `modelturn.go`)

4. **The Heads-up method and notes.**
   - The method text, placed before the notes in `modelTurn.Run` for the
     Heads-up job.
   - Default notes, the `schedule_get` label, the guidance on editing notes,
     and the call budget.
   - `job_reports` with `days`.
   - Tests updated for the new composition.
5. **Docs and settings.**
   - `docs/architecture.md`: the Heads-up, route check, and weather sections.
   - `docs/operations.md`: the new settings.
   - `README.md` if the Heads-up text changes.
   - `.env`, `deploy/.env.nas`, and the plan index.

## Files affected

- `internal/config/conversation.go`, `internal/wiring/wiring.go`,
  `internal/app/app.go`
- `internal/turn/scheduled.go`, `internal/task/service.go`,
  `internal/modelcatalog/`
- `internal/schedule/modelturn.go`, `headsup.go`, `tools.go`, `reports.go`
- `internal/transit/services.go`, `internal/connectors/ns/`, `internal/routes/`
- `internal/connectors/web/weather.go`
- Tests beside each.
- `docs/architecture.md`, `docs/operations.md`, `README.md`, `.env`,
  `deploy/.env.nas`, `docs/plans/README.md`

## Validation

- `gofmt`, a `go mod tidy` diff, `go vet ./...`, `go test ./...` and
  `go test -race ./...`, `go build ./...`, and `scripts/check-plans`.
- Integration tests:
  - A Heads-up run uses the Heads-up model, and a job's own model wins over
    it. A model the catalog dropped falls back to the chat default.
    `schedule_update` refuses a model the catalog does not offer.
  - A Heads-up run's message carries the method before the notes.
  - A route with a partly cancelled train continues on an NS alternative,
    with leave time, arrival, and verdict. A missing departure is named as
    one to replace.
  - `job_reports` with `days` returns full texts. Without it, the listing is
    unchanged.
  - An address resolves to its town.
- Live, after the operator deploys:
  - The morning Heads-up on an office day covers the trip there and the trip
    back, after the last event.
  - A cancelled saved train comes with an alternative.
  - The evening run has no departures that have already left.
  - A device wear level is not repeated unless it changed.
  - Correcting a report in chat changes one note.

## Risks and open questions

- A stronger model at high effort costs more of the user's subscription and
  takes longer.
  - Two runs a day is small.
  - Scheduled runs hold the principal's turn lane, so a chat message sent
    during the 07:30 run waits up to the new time limit (3m). Running
    scheduled turns without the lane is a follow-up if that wait shows.
- A job that sets `model` cannot be read by an older image, since the config
  decoder refuses unknown fields. Rolling back would turn every run of such a
  job into a config failure until the field is removed.
- The method is still a prompt. A stronger model and tools that already
  return alternatives make it far more likely to be followed, but not
  certain. A critique pass on the draft is a follow-up if live checks still
  miss trips.
- Showing the run the user's replies to earlier reports is not done here.
  Chat text is not kept in the database after delivery.
