# 0116 — Extra transfer time for NS trip planning

Status: `done`

## Context

The NS app lets a traveller add extra transfer time, and NS then plans only
connections that leave that much more time at each change between trains. The
`v3/trips` endpoint takes the same setting as `addChangeTime` (plan 0091).

Plan 0091 uses `addChangeTime` only inside departure checks, as a retry when a
named train is not found. Free trip planning with `ns_trips` cannot ask for it,
and its schema does not mention it. In a recent conversation, a user asked
many times for a route with longer train-to-train transfers. The model only
had `via` and the departure time to work with. It read "transfer" as the wait
between a bus and a train, rewrote a scheduled job instead of planning the
trip, offered `via` as if it added transfer time, and then agreed that the
feature existed without using it. With extra transfer time, NS would have
planned the train the user had in mind. NS leaves that train out of its plain
plan because a later train with a 2-minute change arrives at the same time.

## Goal

When a user asks for more time at transfers between trains, or a plan's
train-to-train transfer is too short, the model asks NS for it directly. It
gets back the trips NS plans with that much extra time at every transfer,
including trains the plain plan leaves out.

## Decisions

- **One optional `ns_trips` field, `extra_transfer_minutes`.** It is sent as
  `addChangeTime`, and is allowed from 1 to 30 minutes. 30 is where the
  departure-check retries stop, and it is enough for any change on foot. Zero
  or leaving it out keeps NS's own plan.
- **It works with free planning and `arrive_by`, but not with `departures`.**
  A departure check already retries with 10, 20, and 30 extra minutes. The
  error says so, so the model drops the field instead of stacking the two.
- **The result header names the setting.** For example, "with 10 extra
  minutes for each transfer". The model then reports the transfers as NS
  planned them and does not present the added minutes as the real transfer
  time.
- **The guidance says when to use it, and what it is not.** It is the NS
  app's extra transfer time, for changes between trains only. It is not the
  wait between a bus and a train, and `via` only picks a station to travel
  through.
- **No per-route or per-user default yet.** A saved route that always wants
  longer transfers is a follow-up, if this proves useful.

## Approach

1. **Field.** In `internal/connectors/ns/tools.go`, add the schema field and
   decode and validate it. Pass it to `planTrips`, which already sends
   `addChangeTime` and keys the cache on it. Reject it with `departures`.
2. **Rendering.** `renderTrips` adds the extra minutes to the header line.
3. **Guidance.** Add one sentence to the NS tool guidance.
4. **Tests.** An integration test against the fake NS server: the query
   carries `addChangeTime`, the header names it, a value out of range and the
   combination with `departures` are refused, and a plain call still sends no
   `addChangeTime`.
5. **Docs.** `docs/architecture.md` (departure checks section) and this plan's
   index row.

## Files affected

- `internal/connectors/ns/tools.go`, `render.go`, `service_test.go`
- `docs/architecture.md`, `docs/plans/README.md`

## Validation

- `gofmt`, a `go mod tidy` diff, `go vet`, `go test ./...` and
  `go test -race ./...`, `go build`, and `scripts/check-plans`.
- A live check after deploy: ask for a route with longer transfers between
  trains, and see `ns_trips` called with `extra_transfer_minutes` and a
  train the plain plan leaves out.

## Risks and open questions

- The model may still not reach for the field. The guidance names the phrases
  users say, and the field description repeats the NS app setting by name.
- NS may move a later leg to a later connection when extra time is added. The
  header states the setting, so the reply can say why.
