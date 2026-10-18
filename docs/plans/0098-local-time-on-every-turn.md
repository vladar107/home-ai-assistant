# 0098 — The current local time on every model turn, and time zones that hold

Status: `done`

## Context

The model must get the current local date, weekday, time, and time zone in
every turn's message, chat and scheduled alike. Chat turns and background
tasks add a `[system] Current local time: …` line. Unattended turns do not:
scheduled model-turn jobs, trip scans, and agent watch checks all run through
`runUnwatched`, which adds no time line. Scheduled jobs and trip scans only
say they are "running for" the slot they were scheduled for. After downtime,
a run can start hours after its slot, inside the job type's catch-up window,
so the model can believe it is 08:00 when it is 13:00. The time is also
written in about five places, in slightly different forms. Some name the
zone and some do not, and the watch check falls back to the server's zone
when the watch's zone does not load.

Several date calculations also use the wrong zone:

- When a watch is proposed or changed, its "First check:" line adds days to
  the current time in the server's zone (UTC in the container), not the
  watch's zone. Across a change to summer time, a day can be skipped.
- An hourly weather forecast for coordinates bounds its date by today in
  UTC, because coordinates come without a zone. Open-Meteo answers in the
  place's zone, so just after local midnight the furthest valid day is
  refused.
- Backlog dates are written in UTC, in the tool results and in the
  `/backlog export` text.
- A route watch passes the end of its window, an instant in the watch's
  zone, to the routes service, which reads the day in the household zone. A
  watch kept in another zone can check the wrong day.
- Zone loading is duplicated. `watch.loadZone` copies `schedule.LoadZone`,
  and the trip package's `loadZone` silently falls back to UTC while still
  labelling times with the trip's zone.

## Goal

Every model turn that acts for the user carries one unambiguous
current-time line, with the date, weekday, time, zone abbreviation, and
IANA zone name in the household zone. Unattended turns still say which slot
they were scheduled for, and say plainly that the current time is the one
to go by. Each date calculation listed above uses the zone it is about.

## Decisions

- A new `internal/localtime` package owns zone loading (`LoadZone`), the
  shared time format (`Format`), and the turn line (`Line`).
  `schedule.LoadZone` delegates to it, so the scheduler's many callers stay
  unchanged.
- The conversation service adds the time line on every model turn path:
  chat, background task, and unattended (scheduled job, trip scan, watch
  check). It is always the household zone, from `HOME_AGENT_TIMEZONE`.
- Scheduled jobs and trip scans say "scheduled for <slot>" in the job's own
  zone, with the zone named, and tell the model to go by the current local
  time above. An agent watch check names its own zone and the time there,
  because a watch can keep another zone.
- Zones stored on records were validated when they were saved. If one no
  longer loads, display falls back to UTC, and the label names UTC rather
  than the stored zone. Inputs are still refused with an error.
- Tab naming and alert rewriting stay without a time line. They have no
  tools, act on given text, and must not add dates to it.
- An hourly forecast for coordinates bounds its date in the household zone.
  A geocoded place keeps its own zone. UTC is used only when neither is
  known.
- A route watch passes the start of its window, whose calendar day is the
  occurrence's day. The end can be midnight at the start of the next day,
  so it is not used. The routes service
  now reads a given date's calendar day in that date's own zone; the
  route's clock times stay in the household zone.

## Approach

1. Add `internal/localtime`, and point `schedule.LoadZone`, the watch
   package, and the trip package at it.
2. Build the time line in one conversation helper, used by `turnMessage`,
   `RunTask`, and `runUnwatched`.
3. Reword the scheduled-job and trip-scan headers, and the agent watch
   check header, using `localtime.Format`.
4. Fix the watch's "First check:" day stepping, route watch day selection,
   hourly forecast bounds, and backlog dates. The web connector and backlog
   get a household `Location` in their config.
5. Add regression tests, crossing a daylight saving change where it
   matters.

## Files affected

- `internal/localtime/`
- `internal/conversation/service.go`, `task.go`, `scheduled.go`
- `internal/schedule/cron.go`, `modelturn.go`
- `internal/trip/` (zone loading, scan header, zone labels)
- `internal/watch/` (zone loading, check header, next window, route watch)
- `internal/routes/service.go`
- `internal/connectors/web/` (config, hourly bound)
- `internal/backlog/`, `internal/telegram/backlog.go`
- `internal/app/app.go` (wiring the household zone)

## Validation

- Integration tests: an unattended turn's message carries the time line;
  a late scheduled run says both its slot and the current time; a watch
  proposed just before a change to summer time names the next day; a route
  watch in a zone ahead of the household checks its own day; an hourly
  forecast for coordinates just after local midnight accepts the furthest
  day; backlog dates follow the household zone.
- `gofmt`, `go mod tidy -diff`, `go vet`, `go build`, the full race test
  suite, and `scripts/check-plans`.

## Risks and open questions

- Watch checks in a zone other than the household's get two times, each
  with its zone named. This is clear, but slightly redundant when the zones
  match.
