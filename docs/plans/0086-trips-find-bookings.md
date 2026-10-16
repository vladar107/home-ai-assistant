# 0086 — Trips that find the bookings in mail and calendar

Status: `done`

## Context

Plan 0082 shipped trip tracking. The user had a trip booked in their mail and
calendar, but the assistant found nothing, and the trip was added by hand.
The production logs for 2026-10-16 show three causes:

- **The Trips scan never ran.** It is a built-in job that is off until the
  user turns it on in `/settings`. No `trip.scan` run appears in the last
  three days.
- **Chat does not look.** Asked about trips, the model called `trip_list`,
  got an empty list, and stopped. When it later added the trip, it ran one
  `gmail_search`, read only Gmail's snippet (it never called
  `gmail_read_message`), and never called `calendar_events`. The trip
  guidance only says to record trips the user mentions.
- **The scan would have looked too short a way back.** `ScanPrompt` searches
  mail received in the last 30 days. Travel is often booked months ahead, so
  the confirmation is older than that.

## Goal

- Turning Trips on scans at once, and the user can rescan at any time.
- The scan finds bookings confirmed up to 180 days ago and reads each
  candidate message in full before it reports it.
- In chat, asking about trips makes the model look in mail and calendar
  before it answers that there are none.
- When the scan is off, the model says so and offers to turn it on.

## Decisions

- **Look back far on the first scan, then near.**
  - A new STRICT table `trip_scans` keeps the start time of each principal's
    last successful scan. It cascades with the principal.
  - The prompt's mail window is computed in code. It is 180 days when there
    is no successful scan or the last one is more than 7 days old, and
    otherwise 8 days, so a day missed by a restart is still covered.
  - The calendar window stays at the next 180 days. `calendar_events` covers
    at most 62 days per call, so the prompt asks for three calls.
  - A run counts as successful when its reply decodes, even if the tool
    budget ran out first. An unreadable reply or a failed run records
    nothing. A run whose cards could not be sent records nothing either,
    because its finds are forgotten and the next scan must search as far
    back to offer them again.
  - A run counts only when the principal's mail could be read as it started:
    Mail granted and on for an account that needs no reconnect. A run with
    only Calendar readable still runs and offers cards, but it searched no
    mail, so it leaves the next scan searching far. The scan handler asks
    through a `MailCheck` interface, which `internal/app` implements.
- **Read in full, within budget.**
  - The prompt names `gmail_read_message` and `calendar_read_event`, and it
    says never to report a booking from a snippet alone.
  - It asks for several searches: booking words in English and in the
    user's language, and the senders of airlines, rail, hotels, and rentals.
  - The budget is 40 calls on a near window and 60 on a far one.
  - The prompt says that when tools are refused for budget, the model replies
    in the same JSON with what it has. A partial far scan still counts, and
    the near scans that follow pick up what is new. Nothing found is offered
    twice, because deduplication by booking fingerprint stays in code.
- **Turning it on runs it.**
  - The Trips switch in `/settings` queues a run through `RunNow` both when
    it creates the job and when it re-enables a paused one.
  - While the scan is on, the section also gets a *Scan now* button. A manual
    run with nothing new says so, as today.
  - A manual run that sent cards or update lines says how many, in counts
    only, counting an update only once its line was sent, such as "Found 2 new trips in your mail and calendar; their cards
    are above." Before, the scheduler followed the cards with "Nothing to
    report right now." `schedule.Occurrence` gains `Manual` so the handler
    knows. Scheduled runs stay quiet.
  - The run is named by the button press, so a retried Telegram update
    queues no second run. Settings sections get the press through the
    context, because their `act` has no parameter for it.
  - No two scans run at once for one user. While the Trips job has a run
    queued or running, turning it back on and *Scan now* queue nothing and
    say a scan is already running. The section asks the new
    `schedule.Service.Busy`, so `RunNow` keeps its meaning for `/jobs`.
- **The model knows when the scan is off.**
  - A `trip_list` listing ends with one line when the scan could run but does
    not: the scan is off and can be turned on in `/settings`, or the model
    can look now. That is when Google mail or calendar is readable for the
    principal (the same test as the `/settings` section) and there is no
    enabled Trips job.
  - The line appears on chat turns only. Scheduled and task turns, such as
    Heads-up, never get it, so a user who chose the scan off is not reminded.
  - The trip tools read this through a small `ScanState` interface, set with
    `SetScanState`. `internal/app` implements it, and `MailCheck`, in one
    value over the schedule service and one read of the Google account
    status, which it gets once the Google connector exists. `internal/trip`
    gains no new imports. If the state cannot be read, the line is left out
    and a warning is logged.
- **Chat looks before it says "none".**
  - The list guidance, which every turn with trip tools gets, says this.
    When the user asks about their trips, or asks to add one that
    `trip_list` does not have, and mail or calendar tools are available:
    1. Search mail and calendar first.
    2. Open each likely booking with `gmail_read_message` or
       `calendar_read_event`.
    3. Report or add what was found.
  - A passing mention of travel does not trigger a search, because reading
    mail marks the conversation as having read outside content.
  - A write after the search is proposed for Confirm, as plan 0082 decided.
- **Nothing else changes in plan 0082.** Cards with Track and Ignore stay,
  suggested trips still need a press, and the Ignore and prune rules stay as
  they are.

## Approach

### Chunks

1. **Scan window and reading.** Jev: complex (1.00).
   - Migration 033 adds `trip_scans`, with store methods to read and record
     the last scan. The number follows plan 0085's migration 032, which lands
     first.
   - `ScanPrompt` becomes a function of the window. Add the read-in-full,
     search, and budget wording, and the two budgets.
   - `ScanJobType` takes the scan store, wired in `internal/app/app.go`.
   - Acceptance, from integration tests with a fake model and real
     temporary SQLite:
     - The first run's prompt asks for 180 days on the larger budget.
     - A run a day after a successful one asks for 8 days.
     - An unreadable reply records nothing.
     - A reply after an exhausted budget records the run's start.
     - The same booking found under both windows is offered once.
2. **Switch, Scan now, and guidance.** Jev: undecided (0.31); classified as
   complex because the guidance wording steers model behaviour and the switch
   reaches into the scheduler. Depends on chunk 1, because both edit
   `internal/app/app.go`.
   - In `internal/telegram/trips.go`: run now on create and re-enable, and
     *Scan now*.
   - `ScanState`, its implementation in `internal/app/trips.go`, and the
     `trip_list` line.
   - The list guidance in `internal/trip/tools.go`.
   - The README, with "turn it on" and "Scan now".
   - Acceptance, from integration tests against a fake Telegram boundary:
     - Turning Trips on, and turning it back on after a pause, each queue one
       manual run.
     - *Scan now* queues one.
     - `trip_list` carries the scan-off line exactly when Google mail or
       calendar is readable and there is no enabled Trips job.

### Waves

- Wave 1: chunk 1.
- Wave 2: chunk 2.

## Files affected

- `internal/trip/`: `scan.go`, `tools.go`
- `internal/store/sqlite/`: `migrations/033_trip_scans.sql`, `trips.go`
- `internal/telegram/`: `trips.go`, `settings.go`, `service.go`
- `internal/schedule/`: `schedule.go`, `service.go`
- `internal/app/app.go`, `internal/app/trips.go`
- `README.md`, `docs/operations.md`, `docs/plans/README.md`

## Validation

- `gofmt`, a `go mod tidy` diff, `go vet`, `go test -race ./...`,
  `go build`, and `scripts/check-plans`.
- A live check on the operator's GoLand server:
  - First cancel the trip that was added by hand, because it has no booking
    reference to merge with.
  - Turn Trips on, and see a card for the missed booking.
  - Press *Scan now*, and see no second card.
  - In chat, ask about upcoming trips, and see the model search and read
    mail and calendar.

## Risks and open questions

- A wider window costs more calls on the first scan. The daily scans after
  it stay small.
- A booking in a language or format the model does not recognise is still
  missed. The live check uses the user's real booking mail.
- Scans use the user's own model subscription, as before.
