# 0091 — Departure checks that look past NS's preferred trips

Status: `done`

## Context

Backlog item `58a4af1f` asks for this: when a train the user names is not
found, ask NS again with `addChangeTime` before calling it missing.

A departure check (plan 0027, `ns_trips` with `departures`, and
`ns.Service.CheckDepartures` for saved routes and watches) asks
`v3/trips` for trips from the planned minute. Only a trip whose first train
leaves the origin at exactly that minute counts. When none does, the
station's departure board is read for a cancelled train. Anything else is
reported as not in the timetable.

The NS planner drops trips that a later departure beats. If a later first
train still makes the same onward connection, or a later train with a
transfer arrives before a slow direct one, the earlier train is left out of
the plan. It still runs. The user named a real train, and the check reports
it as not in the timetable.

`addChangeTime` (extra minutes for each transfer) changes which trips win.
On 2026-10-16, a live `v3/trips` query was made for a two-leg journey with a
transfer at Utrecht Centraal, from 08:00 on a weekday:

- Without the parameter, and with `addChangeTime=5`, the first trains left
  at :53 and :23.
- With `addChangeTime=10` or `15`, the :08 departure appeared too. It makes
  the same connection that the :23 departure makes.

So NS supports the parameter on `v3/trips`, and it surfaces the trains that
the plain query hides.

## Goal

A planned departure that runs, but that NS's default plan leaves out, is
checked like any other: it gets its live status, platform, and legs. A
departure that is still not found after the retries is reported as not in
the timetable, as today.

## Decisions

- **Retry only for a departure that is not found.** After the plain query
  finds no exact match, ask again with `addChangeTime` 10, then 20, then 30.
  Stop at the first response that has the departure. The board fallback for
  cancelled trains runs only after all three retries find nothing.
  - The smallest step that works is used, so the transfer shown is as close
    as possible to the one the user would make.
  - Free trip planning, without `departures`, is unchanged.
- **The cache key includes the change time.** A response from a retry is
  added to the check's responses, so a later departure in the same check can
  match it. Among matches, the trip that arrives first still wins.
- **Add a note when a retry found the train.** The note says that NS showed
  this train only with N extra minutes per transfer, so the connection shown
  may be later than the tightest one. It is added after any NS message.
- **A failed retry is a failed check.** If a retry request fails, the
  departure is reported as check failed with the error text, not as not in
  the timetable. The train might be in the timetable, and the check could
  not show it either way.
- **The guidance keeps its rule.** The model is still told never to put a
  nearby train in place of a departure that was not found. The connector
  now looks harder before it reports one.

## Approach

### Chunks

1. **Retry ladder.** Done by the orchestrator, since it is one small change
   in one package.
   - In `internal/connectors/ns/check.go`, `checkDepartures` walks the
     ladder before `boardFallback`.
   - `planTrips` takes a change time: 0 leaves the parameter out, and the
     value is part of the cache key.
   - `departureFrom`'s result gets the retry note.
   - Acceptance, from integration tests against the fake NS server with
     recorded `v3/trips` fixtures:
     - A departure missing from the plain response but present with
       `addChangeTime=10` gets its live status, legs, and the note. It asks
       NS exactly twice.
     - A departure that first appears at step 20 asks three times and uses
       the step-20 trip.
     - A departure absent from every step falls through to the board, and
       is reported as cancelled or not in the timetable, as today.
     - A failed retry gives check failed.
     - Two departures in one check, where the second matches a retry
       response, ask NS no more often than needed.
     - `CheckDepartures`, which saved routes and watches call, gets the
       same result as `ns_trips`.
2. **Docs.** The "Departure checks" section of `docs/architecture.md`.

## Files affected

- `internal/connectors/ns/`: `check.go`, `tools.go` (`planTrips`),
  `status.go` and `render.go` (callers), `service_test.go`
- `docs/architecture.md`, `docs/plans/README.md`

## Validation

- `gofmt`, a `go mod tidy` diff, `go vet`, `go test -race ./...`,
  `go build`, and `scripts/check-plans`.
- A live check, coordinated with the operator's GoLand server: ask for a
  departure that is hidden by default, such as the :08 above, and see its
  status instead of "not in the timetable".
- After that, close backlog item `58a4af1f`.

## Risks and open questions

- A departure that is truly missing now costs up to three more NS requests
  before the board fallback. Each one is cached for 30 seconds. Watches check
  one departure per cycle, so their extra load is bounded by the number of
  watches with a wrong time.
- The ladder stops at 30 minutes. A train that is hidden behind a longer
  slack is still reported as not in the timetable.
- With extra change time, NS may move a later leg to a later connection.
  The note says so. A per-leg check of the user's own connection would need
  the saved route's legs and is out of scope.
