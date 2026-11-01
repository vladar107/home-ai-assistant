# 0122 — Jobs relative to events, conditions on any job, and a weather source

Status: `done`

## Context

Scheduled jobs and reminders fire at a clock time: cron for recurring jobs,
`run_at` for one-time jobs (plans 0024 and 0040). The table enforces exactly
one of the two (`CHECK ((cron IS NULL) <> (run_at IS NULL))`, migration 011).
A run row is made only when the scheduler claims a job whose `next_run_at`
is due.

Trip check-in reminders (plan 0082) come close to "relative to an event",
but the lead time is baked into an absolute `run_at` when the reminder is
stored.

Watches (plans 0058, 0068, and 0074) check conditions, but only inside their
own time windows, and they never gate a job.

The user wants requests like these to work:

- "Remind me 30 minutes before the dentist."
- "An hour before every meeting titled Planning, send me the agenda."
- "When my trip starts, tell me the weather there."
- "At 07:40 on weekdays, tell me only if rain is coming in the next hour."

The last one needs rain data a code check can use. The weather tool already
calls Open-Meteo for hourly forecasts. Open-Meteo also offers a 15-minute
forecast (`minutely_15`) that can carry this.

## Goal

- A job can be **anchored** to an event instead of a clock time. It then
  fires at an offset before or after:
  - one calendar event, or each event that matches a calendar filter;
  - a trip's start or end;
  - arriving at or leaving a saved place, once plan 0123 lands.
- Any job can carry a **condition**. When the job is due, the condition is
  checked once. If it does not hold, the run is skipped without a message.
- A new **weather** watch kind checks expected precipitation at a place in
  code, with no model turn. It works as a standalone watch ("tell me when
  rain is coming") and as a job condition.
- When an anchored event moves, the job follows it. When the event is
  cancelled, the job is finished and the user is told in one line.

## Decisions

- **Anchors live in the job config, and the existing claim path runs them.**
  The config gains an optional `anchor`:
  - `calendar_event`: account, calendar ID, and event ID. One event.
  - `calendar_match`: an optional account and calendar, and a title
    substring. Each matching event.
  - `trip`: a trip ID and `start` or `end`.
  - `place` (from plan 0123): a place ID and `arrive` or `leave`. It fires
    on every arrival, unless `once` is set.

  Every anchor has `offset_minutes`: from −2880 to +1440 for calendar and
  trip anchors, and from 0 to +240 for place anchors.

  No run is created ahead of time, because the scheduler executes any queued
  run at once. The anchor resolver instead keeps the job's `next_run_at` and
  a new `occurrence_key` column (such as the event ID and start) on the job.
  The existing claim path then makes the run when it falls due.
- **Two storage shapes, one migration.**
  - *Single-occurrence anchors* (`calendar_event`, `trip`) are one-time
    jobs. They keep `run_at`, which the resolver moves, and are finished
    after their run as one-time jobs are today. They need no schema change.
  - *Repeating anchors* (`calendar_match`, `place`) have neither cron nor
    `run_at`. A new nullable column `anchor` (`repeating`) admits them. The
    checks become: exactly one of `cron`, `run_at`, or `anchor = 'repeating'`;
    and `finished_at` only with `run_at`.

  - `finished_at` is also allowed with `anchor = 'repeating'`, so a `place`
    job with `once` finishes after its run.

  Changing the checks rebuilds `scheduled_jobs`, which cascades into
  `schedule_runs`, as migration 011 notes. So both tables are rebuilt in one
  migration, keeping every row and every later column: `travel_pause`
  (migration 030), `destination` (037), and the runs' `delivered_chunks` and
  `delivered_messages` (038). The migration test asserts each of them.
  - A `place` job has no `next_run_at` until an arrival or a leave sets it.
    The column stays `NOT NULL`, with the same far-future value the service
    already uses for jobs with no next run (`now + 100 years`).
    `schedule_list`, `schedule_get`, and `/jobs` show "waits for arrival"
    or "waits for leaving" instead of that date.
- **The scheduler learns about anchors.** Writing `next_run_at` is not
  enough. Three places compute the next run from `JobSchedule`, which knows
  only cron and `run_at`, and would pause an anchored job as invalid:
  - `advance()`, through `JobSchedule(job)` and `schedule.Latest`;
  - `SetEnabled`, through `JobSchedule.Next`.

  Each consults the job's resolver for an anchored job. After a run, the
  next occurrence is the resolver's: the next match for `calendar_match`,
  and the sentinel for `place`.
- **Resolvers.** An anchor resolver is registered per kind, as watch
  evaluators are. Calendar and trip resolvers run every 15 minutes for users
  with anchored jobs, and when a job is saved.
  - `calendar_match` looks 14 days ahead and keeps only the next matching
    occurrence on the job. After a run, the resolver moves to the next one.
  - A moved event moves `next_run_at` (and `run_at` for a single-occurrence
    job).
  - A cancelled event finishes a single-occurrence job with `error_kind =
    anchor_gone` and one line in the Reminders feed: "Event cancelled, so the
    reminder is gone". A `calendar_match` job moves to its next match.
  - The resolver confirms again that an occurrence still exists once it is
    within 30 minutes. It does this in its own pass, with a 10-second timeout
    per call. The serial `advance()` loop never calls Google, so one slow
    calendar cannot delay other principals' jobs.
  - Snoozing an anchored reminder pins that occurrence's time. The resolver
    keeps moving the anchor, but never overwrites a snoozed occurrence.
  - A `place` anchor is driven by events, not polled: plan 0123 calls the
    resolver on arrive and leave, and the resolver sets `next_run_at` to the
    event time plus the offset.

  Skip and finish reasons use the existing free-form `error_kind`, so they
  need no migration.
- **Conditions are synchronous checks at one instant.** Watch evaluators are
  stateful and windowed. The agent evaluator returns `ErrSkip` and finishes
  later. So conditions do not call `Evaluate`. Instead, a new `Condition`
  interface takes a spec and an instant and returns met or unmet, a short
  value, or an error. It is implemented for the kinds where that is
  well-defined:
  - `weather`;
  - `home`, as a state comparison without `for_minutes`;
  - `departure`, with an explicit departure time in the condition;
  - `disruptions`, at the instant;
  - `agent`, through a direct synchronous `RunCheck`, which costs one model
    turn. The guidance says to prefer a code-checked kind.

  `route` and `home` with `for_minutes` are refused as conditions, with a
  message explaining why.

  Reminders are `Light` jobs: they never ask the model and skip the
  scheduler's concurrency bound. An `agent` condition on a reminder is
  therefore refused, and code-checked kinds are allowed.
  - When the condition is unmet, the run ends as `skipped` with `error_kind =
    condition_unmet`, and nothing is sent.
  - When the condition is met, its value is added to the job's turn, so a
    report can say "rain from 07:55".
  - A failed check runs the job anyway with a "could not check" note, so a
    reminder is never lost to a failed check.
- **Weather kind.** Its target is a place: coordinates from a saved place
  (plan 0123), the user's current shared location, or a named place geocoded
  as the weather tool already does. Its spec:
  - `within_minutes`: 15–360, default 60;
  - `threshold_mm`: precipitation per 15 minutes, default 0.2;
  - triggers `enter` (rain is expected) and `clear` (it is no longer
    expected).

  It is code-checked from Open-Meteo's `minutely_15=precipitation` for the
  next `within_minutes`. The value is the first 15-minute slot above the
  threshold and that slot's amount. The check interval is 10 minutes, and
  identical requests share a cache for that time. The per-principal watch cap
  applies as today.
- **One watch-table rebuild for this plan and plan 0123.** This plan's
  migration rebuilds the watch tables to admit both `weather` and `place` in
  the kind `CHECK`. It keeps `travel_pause` (migration 030) and every index.
  A migration test asserts that both survive. Tool validation refuses
  `place` until plan 0123 lands, so 0123 needs no second rebuild.
- **Tools.**
  - `schedule_create` and `schedule_update` accept `anchor` and `condition`.
  - `schedule_list` and `schedule_get` show them in words, for example "30
    min before each Planning event, only if rain is expected at Home within
    60 min".
  - `watch_propose` accepts the `weather` kind.
  - Proposals go through the existing card and act-and-tell paths.
- **Calendar reads stay read-only.** Anchors only read events, through the
  existing calendar client and the user's own Google accounts. A calendar
  switched off makes the job's resolver fail, and the existing job failure
  notice tells the user.
- **Settled during implementation.** These refine the decisions above where
  the code asked for a concrete choice.
  - The creation tools are `schedule_propose` and `reminder_set`; both take
    `anchor` and `condition`, and `at` becomes optional for a reminder.
    `schedule_update` also takes `remove_condition`. A job keeps its kind of
    timing: a clock job cannot gain an anchor, nor an anchored one cron or
    `at`.
  - The condition is stored in the config as its watch spec with its
    description, so `schedule_list` and `/jobs` can show it without the watch
    service.
  - After a run, `advance()` moves a repeating job to the sentinel, keeps the
    occurrence it ran for as a new `last_occurrence_key` column, and wakes the
    resolver pass, which finds the next match and never returns that
    occurrence again, however the event moves; `advance()` itself never asks a
    resolver. A long occurrence key is stored as its SHA-256 digest, and a
    calendar ID is at most 256 characters.
  - The resolver pass reads each principal's jobs in a worker of its own, at
    most four at once; a principal whose worker is still reading is left to a
    later pass, so one hanging calendar delays only its owner's anchors. A wake
    that comes during a pass is kept for the next one.
  - The failure notice is one line, `anchor_failed`, to the Reminders tab after
    three failed passes in a row; the job keeps its last known time.
  - A snooze clears the job's `occurrence_key`, which marks the time as pinned.
    Snoozing applies to single-occurrence reminders, as it does today to every
    one-time reminder; a repeating reminder is sent without snooze buttons.
  - A condition's value may come from outside, so the check reports whether it
    is private or outside content, and the run's report is marked as such. A
    model turn gets the value quoted as untrusted data, not as a `[system]`
    line. A reminder's text is sent as written and is never outside content,
    so a reminder takes only weather and home conditions, whose values code
    writes.
  - A weather check reads enough 15-minute slots to cover the longest window
    from a forecast reused for 10 minutes, and a forecast that stops short of
    the window fails the check rather than reporting it dry. A failed forecast
    request reports only its class, never the coordinates in its address.
  - The watch package's interface is `InstantChecker`, because `Condition` is
    already the home watch's condition type. The home check lives in the Home
    Assistant connector beside its evaluator.
  - A manual Run now skips the condition.

## Approach

1. Schedule: the anchor and condition config and validation, the jobs and
   runs rebuild migration with its test, `occurrence_key`, the resolver
   registry, and claim-time confirmation.
2. Calendar and trip resolvers in their connectors, registered in
   `internal/wiring`. A stub `place` resolver that refuses until plan 0123.
3. Watch: the `Condition` interface and its implementations, the `weather`
   kind and evaluator, the Open-Meteo `minutely_15` client in the web
   connector, the watch-table rebuild admitting `weather` and `place`, and
   tool schemas.
4. Guidance: when to anchor instead of using a clock time, and to prefer
   code-checked conditions.
5. README, `docs/architecture.md`, and `docs/operations.md`.

## Files affected

- `internal/store/sqlite/migrations/047_anchored_jobs.sql` and
  `048_weather_and_place_watches.sql`, with `schedules.go` (claim and finish
  queries) and the watch store
- `internal/schedule`: the job model, `service.go` (`advance`,
  `SetEnabled`, snooze), tools and their rendering, and a new `anchor.go`
- `internal/telegram/jobs.go` (next-run rendering)
- `internal/watch`: `watch.go`, `tools.go`, and new `condition.go` and
  `weather.go`
- `internal/connectors/web/weather.go`, `internal/connectors/calendar`,
  `internal/trip`, `internal/turn` (`RunCheck` used synchronously)
- `internal/wiring`, `internal/prompt`, README, and docs
- `internal/connectors/homeassistant/watch.go` (the home condition check) and
  `internal/testutil/calendarsim` (moving and cancelling an event)

## Validation

- Tests with a fake clock, a temporary SQLite store, a fake calendar API at
  its HTTP boundary, and a fake Open-Meteo server:
  - a reminder 30 minutes before an event fires once, at the right time;
  - moving the event moves the job;
  - cancelling the event finishes the job and sends one line;
  - a `calendar_match` job runs for each matching event in turn and never for
    other events;
  - no anchored run is created before it is due;
  - an unmet condition skips silently, and a met one runs and names its
    value;
  - a failed condition check still runs the job with a note;
  - `route` and `home` with `for_minutes` are refused as conditions, and an
    `agent` condition on a reminder is refused;
  - after a run, `advance()` keeps an anchored job enabled and moves it to
    its next occurrence;
  - enabling an anchored job again sets `next_run_at` from its resolver;
  - a `place` job with `once` finishes after its run;
  - a snoozed anchored reminder keeps its snoozed time when the event moves;
  - a slow calendar API does not delay another principal's due job;
  - `/jobs` shows "waits for arrival" for a `place` job;
  - a weather watch alerts once when rain enters the window and clears when
    it leaves;
  - the jobs migration keeps every existing job and run, and the watch
    migration keeps every watch, state, alert, and `travel_pause`;
  - another principal's calendars and jobs are never resolved.
- A live check from the GoLand server: anchor a reminder to a test calendar
  event, then move the event and see the reminder follow it.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- Calendar polling every 15 minutes adds Google API calls for each user who
  has anchored jobs. Only users with such jobs are polled.
- The precision of the 15-minute forecast differs by region. The threshold
  is adjustable, and the alert says it is a forecast.
- Rebuilding the jobs and runs tables is this plan's riskiest step. It runs
  in one transaction like every migration, and the operator keeps the
  pre-deploy data backup until it is verified (operations guide, section 7).
- Plan 0129 should export anchors and conditions with their jobs.
