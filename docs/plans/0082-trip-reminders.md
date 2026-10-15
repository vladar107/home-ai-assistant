# 0082 — Smart trip reminders

Status: `done`

## Context

Backlog item `5d2c31f8` asks the assistant to track trips: use where the user
is, remind them to check in for their bookings, and pause their regular
reminders while they are away.

The assistant knows nothing about trips today:

- Location comes only from Telegram shares (plan 0043). Only the latest fix is
  kept, and no home point exists.
- Google Calendar (`calendar_events`) and Gmail (`gmail_search`) can be read,
  but nothing turns a booking confirmation into anything lasting.
- The only way to pause a job is `Job.Enabled`, which is the user's own switch.
  Watches have `Enabled` and a one-day `MutedOn`. Neither has a travel state,
  so a cleaning reminder still fires while the user is abroad.
- Scheduled runs get read tools only (`Broker.ReadOnlyToolsFor`). A writer
  that reads mail therefore cannot act on its own, and it should not: mail is
  outside content.

## Goal

- A trip the user mentions in chat, or one found in their mail or calendar and
  accepted with one press, is recorded with its dates, destination, and
  bookings.
- Each booking with a check-in or pickup time (flight, hotel, train, car)
  reminds the user in their own chat when check-in opens.
- While a trip is active, recurring jobs and watches marked
  *pause while traveling* stay silent. They resume by themselves when the trip
  ends, with one message at the start and one at the end saying what was
  paused and what resumed.
- Every turn knows about the active or next trip, so answers fit where the
  user is. The model compares the trip with the shared location when one is
  fresh.

## Decisions

- **Sources: chat, mail, and calendar.** The user can tell the assistant about
  a trip in chat, and a daily scan reads Gmail and Calendar. Location does not
  start or end a trip. It only reaches the model beside the trip line, and the
  model offers to end a trip early when the location says the user is back. A
  configured home point is out of scope.
- **"Check-in on the main account" means in the user's own chat.** Each
  principal gets their own reminders in their own active chat. Nothing is sent
  to another household member.
- **Nothing from mail or calendar becomes a planned trip without a press.**
  - The scan stores each new candidate as a `suggested` trip. A suggested trip
    is not active, sets no reminders, and is not shown in the trip line.
  - Each new suggested trip is sent once as a card with *Track* and *Ignore*
    buttons. A card that cannot be sent drops its suggestion, so the next
    scan offers it again. Suggested trips never reach the model, and they
    have their own cap, apart from the cap on planned trips.
    Track makes the trip `planned` and sets its reminders. Ignore makes it
    `dismissed`, and a dismissed trip is never offered again.
  - A suggested, dismissed, cancelled, or past planned trip is pruned 30 days
    after it ends. Its fingerprints go with it, but by then its bookings are
    in the past and are not found again.
- **Chat tools follow the backlog pattern.** They act directly in chat, and
  after outside content they propose for Confirm.
- **The scan is a built-in job, like Heads-up.**
  - It is job type `trip.scan`, named "Trips". Its ID is derived from the
    principal, and it runs daily at 07:30 (`TripsCron`).
  - It has a switch in /settings. It is off by default and offered only when
    Google mail or calendar is enabled.
  - The handler lives in `internal/trip` and implements `schedule.Handler`, as
    `modelturn.go` does. It calls `RunCheck` with a call budget of 30, which
    leaves room for check-in lookups on the web. It takes no turn slot,
    stages nothing, and asks for strict JSON.
  - Code validates and deduplicates the result, sends the cards itself, and
    returns `Result{Quiet: true}`. A manual run with nothing new reports
    nothing to report.
- **Deduplication by booking fingerprint.**
  - Each booking has a fingerprint computed in code, never supplied by the
    model: the kind plus the normalised reference, or the calendar event ID,
    SHA-256 hashed. It is unique per principal across every status. Chat
    bookings with a reference get one too, so a trip told in chat is matched
    when its booking later turns up in mail.
  - The scan matches a candidate to an existing trip by any of its booking
    fingerprints. A dismissed match is ignored. A planned or suggested match
    is merged booking by booking: matched bookings are updated in place, new
    ones are added, and bookings the user added are kept. The trip's dates
    widen to cover its bookings, and the scan sends one line saying what
    changed.
- **Check-in reminders are ordinary one-time `reminder` jobs.**
  - Each reminder's ID is derived from the trip and booking, so syncing is
    idempotent. The reminders show in /jobs.
  - A reminder is set only when check-in is in the future. If check-in has
    already opened, the tool reply or card says so instead. A reminder that
    already fired, and was pruned, is therefore never recreated.
  - Changing a booking moves its reminder. Cancelling or dismissing a trip
    removes its reminders.
  - If `ErrTooManyJobs` is hit, the trip is still saved, and the reply names
    the reminders that could not be set.
  - The check-in time comes from the first of these that gives one:
    1. The booking itself, if it states when check-in opens.
    2. The provider's own pages, looked up with the web tools: the airline's
       online check-in rules, the hotel's check-in time, or the rental's
       pickup terms.
    3. A default of 24 hours before the booking starts, which the model states
       as its assumption instead of asking (plan 0081).
  - Each booking records which of these the time came from (`stated`,
    `looked_up`, or `default`). Cards and replies show the source.
  - Times are formatted in the booking's zone. Trips never pause these
    reminders.
- **Travel pause is a gate, not a toggle.**
  - Recurring jobs and watches get a `travel_pause` column
    (`INTEGER NOT NULL DEFAULT 0 CHECK (travel_pause IN (0, 1))`). On watches
    it sits on `Watch` beside `Enabled`, not inside the JSON `Spec`, so pending
    approvals still decode.
  - In `advance`, a flagged job whose occurrence (`latest`, not `now`) falls
    inside an active trip moves forward with no run queued, which is the
    existing missed path. It is logged as `schedule.run.travel_paused` and
    counted by its own counter.
  - The watch cycle skips flagged watches at check time, and `deliver` drops
    their queued alerts during a trip.
  - `RunNow` bypasses the gate. `Enabled` is never touched, so the user's own
    pause survives and nothing has to be restored. Missed occurrences are not
    replayed.
  - One-time jobs and Heads-up are never flagged.
- **The flag is set by the model and shown to the user.**
  - `schedule_propose`, `schedule_update`, and the watch create and update
    tools take an optional `pause_while_traveling`. `schedule.Prepare`'s
    signature stays as it is: the flag is set on `Prepared.Job` after Prepare,
    and `Update` keeps it.
  - The guidance tells the model to set the flag for home-bound things such as
    cleaning, bins, the commute, and home devices.
  - When a trip is added or tracked, the model offers to flag the existing
    home-bound jobs and watches, naming them.
  - /jobs and /watches show the flag and let the user toggle it.
- **Heads-up moves to 07:30 and 18:30.**
  - `HeadsUpCron` becomes `30 7,18 * * *`, so the morning Heads-up runs at the
    same time as the Trips scan. One cron expression cannot hold 07:30 and
    18:00, so the evening run is at 18:30.
  - Existing Heads-up jobs still on `0 8,19 * * *` are moved once at
    scheduler start through `Service.Update`, which recomputes their next run.
    A job whose schedule the user changed is left alone.
- **A trip is active while `starts_at <= now < ends_at`**, and its status must
  be `planned`. Any active trip counts, so overlapping trips are fine. Ending a
  trip early sets `ends_at` to now.
- **Start and end notices.**
  - A minute loop marks `started_notified_at` and `ended_notified_at`, so each
    notice is sent once across restarts.
  - Cancelling an active trip sends the end notice at once. Cancelling a trip
    that has not started sends nothing.
- **Text from outside is constrained.**
  - Title, destination, booking title, and reference are each a single line,
    capped (title and destination at 80 runes, reference at 40), stripped of
    control characters, with no URLs.
  - Cards and notices are sent without markup and without link previews.
- **Private data stays out of logs and metrics.** Logs carry IDs only. Metric
  labels are fixed outcome words. Tests and docs use neutral example trips.

## Approach

### Contracts (written by the orchestrator before wave 1)

`internal/schedule` and `internal/watch` each define:

```go
type Travel interface {
	Traveling(ctx context.Context, principalID string, at time.Time) (bool, error)
}
```

Both services gain:

- `SetTravel(Travel)`. A nil `Travel` means the user is never traveling.
- `TravelPausedNames(ctx, principalID) ([]string, error)`, which returns the
  enabled flagged jobs or watches.

`internal/trip` imports `schedule`, never the reverse, and defines:

```go
type Reminders interface { // adapter over *schedule.Service, in internal/trip
	SetReminder(ctx context.Context, principalID, id string, at time.Time, zone, text string) error
	RemoveReminder(ctx context.Context, principalID, id string) error
}
type Paused interface {
	TravelPausedNames(ctx context.Context, principalID string) ([]string, error)
}
type Sender interface { // plain text to the principal's active chat
	Send(ctx context.Context, principalID, text string) error
}
type CardSender interface { // Track/Ignore card, implemented in internal/telegram
	SendTripCard(ctx context.Context, trip Trip) error
}
```

The `trip.Service` API is complete after stage 1:

- Writes: `Add`, `Update`, `Cancel`, `EndNow`, `Suggest` (dedupes by
  fingerprint and returns new, updated, or known), `Track`, `Dismiss`.
- Reads: `Trips`, `Trip`, `Traveling`, `TurnLine`.
- Notice support: `DueNotices(ctx, now)`, `MarkStarted`, `MarkEnded`.
- Maintenance: `Prune(ctx, now)`.

Each write syncs the trip's reminders.

### Stages

1. **Trip core.** Files: `internal/trip/trip.go` and `service.go`,
   `internal/store/sqlite/trips.go`, `migrations/029_trips.sql`, and
   `internal/schedule/trips.go`, which holds only `TripsName`, `TripsCron`,
   `TripsJobID`, and `TripScanType`.
   - Tables `trips` and `trip_bookings` (STRICT, cascade on principal).
     - `trips`: id, principal_id, title, destination, starts_at, ends_at,
       zone, status `suggested|planned|dismissed|cancelled`,
       source `chat|mail|calendar`,
       started_notified_at, ended_notified_at, created_at, updated_at.
     - `trip_bookings`: id, trip_id, kind `flight|hotel|train|car|other`,
       title, reference, fingerprint (unique per principal), starts_at,
       check_in_at, and check_in_source
       `stated|looked_up|default`.
   - Caps: 30 unfinished trips and 20 bookings per trip. Text limits as in
     Decisions.
   - Acceptance:
     - A planned trip with two bookings creates two reminders.
     - Moving a booking moves its reminder. Cancelling the trip removes its
       reminders.
     - A check-in in the past sets no reminder.
     - `Suggest` returns known for a repeated fingerprint, including a
       dismissed one, and updates a planned trip whose dates changed.
     - `Traveling` is true exactly on `[starts_at, ends_at)` of a planned
       trip.
2. **Travel pause.** Files: `migrations/030_travel_pause.sql`,
   `internal/store/sqlite/schedules.go` and `watches.go`,
   `internal/schedule/schedule.go`, `service.go`, and `tools.go`,
   `internal/watch/watch.go`, `service.go`, and `tools.go`, and
   `internal/telegram/jobs.go` and `watches.go`.
   - Acceptance:
     - A flagged recurring job inside a trip advances with no run queued, and
       fires at its first occurrence after the trip.
     - Unflagged, one-time, and `RunNow` runs are unaffected.
     - A flagged watch is neither checked nor delivered during a trip.
     - The tools set the flag and keep it on update, and /jobs and /watches
       toggle it.
     - New Heads-up jobs use `30 7,18 * * *`. At start, an existing job still
       on `0 8,19 * * *` moves to the new schedule with a recomputed next
       run, and a job whose schedule the user changed is left alone.
       (`internal/schedule/headsup.go` is added to this stage's files.)
3. **Trip chat tools.** File: `internal/trip/tools.go`.
   - `trip_add`, `trip_list`, and `trip_update`. Update covers dates, title,
     cancel, end now, and adding, changing, or removing bookings.
   - Writes have an approval executor that re-validates and re-checks the
     caps. `trip_list` is a read tool and is also offered to scheduled runs.
   - The guidance covers recording trips mentioned in chat, choosing a
     check-in time (the booking, then the provider's site through the web
     tools, then 24 hours before), offering to flag home-bound jobs, and comparing the trip
     with the location.
   - Acceptance:
     - A chat write acts at once.
     - After reading an email, the same write is proposed, and Confirm makes
       it.
     - Each reply names any reminder that was not set.
4. **Trips scan.** Files: `internal/trip/scan.go`,
   `internal/telegram/trips.go` (the card and the Track/Ignore callbacks),
   `internal/telegram/service.go` (callback routing and `SetTrips`), and
   `internal/telegram/settings.go` (the switch, following `headsup.go`).
   - Acceptance:
     - The same booking found twice gets one card.
     - Ignore means the trip is never offered again.
     - Malformed or out-of-limit model output is dropped and logged with
       counts only.
     - Nothing becomes planned without Track.
5. **Trip loop.** File: `internal/trip/loop.go`.
   - A minute ticker sends the start notice with the paused names from
     `Paused`, and the end notice through `Sender`. It also runs `Prune`.
   - Acceptance:
     - There is one start notice and one end notice per trip across a
       restart.
     - Cancelling an active trip sends the end notice.
     - A trip that never started sends nothing.
6. **Wiring and docs.** Files: `internal/app/app.go`, and
   `internal/conversation/service.go` (the trip line beside `locationLine`, so
   chat, scheduled runs, and tasks all get it, with the trip zone's local time
   during a trip). Also `README.md`, `docs/architecture.md`, and
   `docs/operations.md`.
   - Acceptance: the app starts, and the turn context shows an active or
     upcoming trip.

### Waves

- Wave 1: stages 1 and 2. They own disjoint files.
- Wave 2: stages 3, 4, and 5. Each owns its own files, and only stage 4 edits
  `internal/telegram`.
- Wave 3: stage 6. It is the only stage that edits `internal/app`.

Each stage ships integration tests for its acceptance, using real temporary
SQLite and fake model and Telegram boundaries.

## Files affected

- `internal/trip/` (new): `trip.go`, `service.go`, `reminders.go`,
  `tools.go`, `scan.go`, `loop.go`
- `internal/store/sqlite/`: `migrations/029_trips.sql`,
  `030_travel_pause.sql`, `trips.go`, `schedules.go`, `watches.go`
- `internal/schedule/`: `schedule.go`, `service.go`, `tools.go`, `undo.go`,
  `headsup.go`, and the new `trips.go`
- `internal/watch/`: `watch.go`, `service.go`, `tools.go`, `undo.go`
- `internal/telegram/`: `jobs.go`, `watches.go`, `headsup.go`,
  `settings.go`, `help.go`, `service.go`, and the new `trips.go`
- `internal/conversation/`: `service.go`, `types.go`, `scheduled.go`,
  `task.go`
- `internal/app/app.go` and the new `internal/app/trips.go`
- `internal/observability/telemetry.go`
- `README.md`, `docs/architecture.md`, `docs/operations.md`

## Validation

- `gofmt`, a `go mod tidy` diff, `go vet`, `go test ./...`, `go build`, and
  `scripts/check-plans`.
- A live check on the operator's GoLand server:
  - Tell the assistant about a short trip and see the check-in reminder in
    /jobs.
  - Flag a job, and see it stay silent during the trip and fire after it.
  - See one start notice and one end notice.
  - Run the Trips scan with a real booking email, and see one card. Ignore
    it, then run the scan again, and see no card.

## Risks and open questions

- The scan relies on the model to read bookings correctly. Wrong dates are
  caught at Track, where the card shows them, but a booking the model misses
  is silently not offered.
- Mail is outside content. A crafted email can at most produce a constrained
  card that the user must press.
- If a trip's time zone is wrong, the trip shifts by hours. Bookings carry
  instants, and the trip's dates follow the bookings when there are any.
- Migration 030 adds columns with defaults. It cannot be undone, but it is low
  risk.
- Location-based trip detection, which needs a home point, is a follow-up.
