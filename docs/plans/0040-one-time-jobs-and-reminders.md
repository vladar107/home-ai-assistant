# 0040 — One-time jobs and reminders

Status: `done`

## Context

[0024](0024-scheduled-model-turns.md) built a durable scheduler whose only
schedule is a five-field cron expression and whose only job type is
`model.turn`. Cron has no year, so "tomorrow at 08:30" cannot be said: the
nearest expression, `30 8 4 10 *`, fires every year. The database enforces this
too — `scheduled_jobs.cron` is `NOT NULL` with a length check — and a job whose
schedule never fires again is parked a hundred years out rather than finished.

The everyday need is the opposite of a recurring report: "remind me at five to
call mom", "in twenty minutes tell me to take the laundry out", and sometimes
"tomorrow at 07:30 check whether my 08:12 train is delayed". The first two need
no model at all; going through one makes a reminder depend on a connected
subscription, remaining quota, and the per-principal turn lock, and costs a
model call to repeat a sentence the user already wrote.

## Goal

A user says in conversation when they want to be reminded of something, and the
assistant immediately replies with the exact local time it will fire, without a
confirmation step. At that time the text arrives in their chat, once, even across
restarts, with Snooze buttons. A user can also ask for a one-time model turn,
confirmed like a recurring job, that runs once and reports. `/jobs` shows pending
one-time jobs beside recurring ones and cancels them; fired ones leave the list.

## Decisions

- **A schedule is either a cron expression or one instant.** `scheduled_jobs`
  gains `run_at`; `cron` becomes nullable; exactly one of them is set. SQLite
  cannot alter a check constraint, so migration 011 rebuilds the table and
  copies the rows; `schedule_runs` is rebuilt with it, because dropping the old
  jobs table would cascade-delete every run. `Schedule` gains a `Once` instant
  beside its cron expression rather than becoming an interface, so the existing
  `Next`, `Upcoming`, and `Latest` serve both kinds: `Next` of a one-time
  schedule returns its instant once and the zero time afterwards.
- **A one-time job finishes rather than disappears.** Deleting it when it fires
  would cascade to the run before it is delivered. Instead the tick that queues
  its run sets a new `finished_at`, the due query skips finished jobs, `/jobs`
  and `schedule_list` hide them, and the existing 30-day prune removes them with
  their runs. Snooze revives a finished reminder in place.
- **Times are given as local wall-clock time.** The model passes `at` as
  `YYYY-MM-DD HH:MM` and an optional IANA zone; it already receives the current
  local time on every turn (0032), so it resolves "tomorrow" and "in 20
  minutes" itself. Go parses the time in the zone, refuses a time inside the
  spring-forward gap with a message asking for another time, takes the earlier
  instant in the repeated autumn hour, and requires it to be in the future and
  at most a year ahead. The time the user is told is rendered by Go from the
  parsed instant into the tool result, which the model is told to repeat as
  given.
- **`reminder.text` is a deterministic job type.** Its configuration is the
  reminder text, at most 500 characters. Its handler returns the text without a
  model call, so it does not take the turn lock, does not need a connected
  subscription, and is delivered even when the quota is spent. It is one-time
  only; recurring reminders stay a `model.turn` until someone needs them.
- **Reminders are created without confirmation.** A reminder can only send its
  owner's own text to its owner's own chat at a time they named, so the
  confirmation buttons add a tap and no protection. `reminder_set` and
  `reminder_cancel` are `RiskWrite` tools that act directly rather than through
  `approval.Propose`; being write tools, they are absent from scheduled runs, so
  an unattended turn cannot create reminders. `reminder_cancel` acts only on
  `reminder.text` jobs; `schedule_remove` keeps its confirmation for model turns.
- **One-time model turns stay confirmed.** `schedule_propose` accepts either
  `cron` or `at`. A one-time `model.turn` uses the same read-only run, prompt
  limits, and confirmation as a recurring one; the one-hour minimum interval
  does not apply to a schedule that fires once.
- **Catch-up is per type.** A missed reminder is still worth receiving, so it is
  delivered however late it is, and one delivered more than five minutes after
  its time says when it was due. A one-time model turn keeps the 6-hour window
  of `model.turn`; a later one is not run and the owner is told so, since unlike
  a recurring job it has no next occurrence to fall back on. A one-time job
  therefore always queues its occurrence, and the run decides.
- **Limits are per kind, and the recurring limit rises from 10 to 50.** A
  principal may hold 50 recurring jobs and, separately, 50 pending one-time
  jobs; finished jobs count toward neither. Ten recurring jobs proved small.
  Runs spend the owner's own subscription, the one-hour minimum interval still
  holds, and one principal's runs already execute one at a time behind the turn
  lock, so a larger limit costs only the owner and, when many jobs share one
  minute, delays their later runs and their chat turns behind that queue.
- **`/jobs` is paged.** A Telegram message carries at most 100 inline buttons
  and 4,096 characters, and up to 100 jobs at three buttons each exceed both.
  `/jobs` shows eight jobs per page — recurring first, then one-time by due
  time — with Previous and Next buttons; callback data carries the page, and a
  page past the end shows the last one. A button press refreshes the page it
  was pressed on.
- **`schedule_list` shortens instructions** to 200 characters per job, so a full
  list stays small in the model's context; the full text is still what runs.
- **Delivered reminders carry Snooze buttons**: 10 minutes, 1 hour, and
  tomorrow at the same local time. Pressing one moves the finished reminder's
  `run_at` and `next_run_at`, clears `finished_at`, and edits the delivered
  message to say "Snoozed until …" without buttons, so a second press on the
  same message does nothing. A snooze of a reminder that was cancelled, pruned,
  or is already pending again edits the message to say so instead.
  Callback data is `remind:<action>:<job id>` with a fixed action vocabulary,
  like `/jobs`. Snoozing needs no new job slot.
- The scheduler cannot import `telegram`, so it delivers a reminder through a
  small `ReminderDeliverer` interface taking the chat, job ID, and text;
  `telegram.ReminderSender` implements it and owns the button labels and
  callback data, so both ends of the snooze vocabulary live in one package.
- **Reminder deliveries look different from reports**: `⏰ <text>` instead of
  `Scheduled: <name>`. A reminder's name is its text, truncated to 60
  characters. Delivered reminders join the next chat turn through the existing
  notice, so "remind me again in an hour" refers to the right one.
- `/jobs` shows one-time jobs as `once, Sat 4 Oct 08:30 CEST`; they have Run now
  and Delete but no Pause, since pausing a single instant only invites missing
  it.

## Approach

1. `internal/store/sqlite/migrations/011_one_time_jobs.sql`: rebuild
   `scheduled_jobs` with nullable `cron`, `run_at`, `finished_at`, the
   exactly-one check, and the due index restricted to unfinished jobs.
   `schedules.go`: read and write the new columns, finish a one-time job in the
   transaction that queues its run, count pending one-time jobs separately,
   revive a finished reminder, and prune finished jobs. Complexity: moderate.
2. `internal/schedule/cron.go` and a new `once.go`: the one-time instant on
   `Schedule`, and the local-time parser with its gap, repeated-hour, and range
   rules. Complexity: moderate.
3. `internal/schedule/service.go`: `Prepare` for either schedule kind, the
   per-kind limits, finishing instead of parking a schedule that never fires
   again, the missed one-time notice, keyboard delivery for reminders, and
   `Snooze`. Complexity: moderate.
4. New `internal/schedule/reminder.go`: the `reminder.text` type and its late
   note. Complexity: simple.
5. `internal/schedule/tools.go`: `reminder_set`, `reminder_cancel`, `at` on
   `schedule_propose`, one-time jobs in `schedule_list`, and guidance on when
   to use a reminder rather than a job. Complexity: moderate.
6. `internal/telegram/jobs.go`, new `internal/telegram/reminders.go`,
   `service.go`, `commands.go`: one-time rows and paging in `/jobs`, the snooze
   callback and its message edit, the command description and privacy line.
   `internal/app/app.go`: register the type and wire the reminder sender.
   Complexity: simple.
7. Documentation: `docs/architecture.md` (the schedule module and the
   confirmation exception), the capabilities in `README.md`, this plan and its
   index row. Complexity: simple.

## Files affected

- new `internal/store/sqlite/migrations/011_one_time_jobs.sql`,
  `internal/store/sqlite/schedules.go`, `internal/store/sqlite/schedules_test.go`
- `internal/schedule/` (`schedule.go`, `cron.go`, `service.go`, `tools.go`),
  new `once.go`, `once_test.go`
- `internal/schedule/` new `reminder.go`
- `internal/telegram/jobs.go`, `internal/telegram/service.go`,
  `internal/telegram/commands.go`, new `internal/telegram/reminders.go`,
  `internal/telegram/conversation_integration_test.go` (the harness registers
  reminders), `internal/telegram/schedule_integration_test.go`, new
  `internal/telegram/reminder_integration_test.go`
- `internal/app/app.go`
- `README.md`, `docs/architecture.md`, `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `scripts/check-plans`.
- One-time parsing, as focused unit tests in `Europe/Amsterdam`: a time in the
  spring-forward gap is refused, the repeated autumn hour resolves to the
  earlier instant, a past time and one more than a year ahead are refused.
- Store, against a real temporary database: migration 011 keeps existing
  recurring jobs and their runs; a one-time job queues one run and finishes in
  the same transaction; finished jobs are neither due nor listed nor counted;
  the recurring and one-time limits of 50 are counted separately; another
  principal cannot snooze or cancel the reminder.
- Integration, through the fake Telegram server and an injected clock: a turn
  calls `reminder_set` and the reminder exists without a confirmation; the clock
  passes its time and the text arrives once with Snooze buttons, without any
  runtime call and with the owner's subscription disconnected; Snooze 10 min
  edits the message and the reminder arrives again ten minutes later; a second
  press on the same message moves nothing and says the reminder is already set; after six or 25 hours of downtime the
  reminder arrives marked late; a one-time `model.turn` is proposed with `at`,
  confirmed, runs once in a read-only turn, and leaves `/jobs`; one missed by
  more than six hours tells the owner it did not run; a scheduled turn is not offered `reminder_set`; with 20
  jobs `/jobs` shows three pages, every page stays within Telegram's button and
  length limits, and pausing a job on page two refreshes page two.
- Live, in the operator's chat: a reminder two minutes ahead arrives on time,
  is snoozed for ten minutes, and arrives again across one restart.

## Risks and open questions

- The tick is 30 seconds, so a reminder fires up to 30 seconds late. A timer
  for the nearest due job would remove the delay if it matters.
- Reminders bypass confirmation, so a model misunderstanding of the time is
  only caught by the user reading the reply. The tool result gives the weekday,
  date, time, and zone rendered by Go, but the model relays it, so a model that
  paraphrases it could still misstate it; `/jobs` shows the stored time.
- Recurring plain-text reminders ("every evening remind me to take my pills")
  would still go through the model. A cron-scheduled `reminder.text` is a small
  follow-up if it is wanted.
