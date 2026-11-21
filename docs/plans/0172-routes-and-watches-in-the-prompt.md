# 0172 — Saved routes and watches in every turn's prompt

Status: `done`

## Context

A chat about the week's trips went wrong turn after turn. The user asked how
to get to work and to two evening events on two weekdays. Saved routes already
covered every leg, with the user's own departures and an arrival target. The
model never called `route_list` or `route_check`. It planned trips with
`ns_trips` instead, starting with the morning leg in the wrong direction, used
up the turn's eight tool calls, and said it had not got to the last trip.

The system prompt carries the principal's memory and a section listing their
enabled scheduled jobs (`internal/schedule/prompt.go`), read each turn.
Routes and watches have no matching section. The model learns that routes
exist only from a general rule in the tool guidance ("look in saved routes"),
and it did not follow that rule. The Jev tool hint, which chooses from tool
descriptions alone, suggested `bus_trip` and `ns_trips`, never
`route_check`.

In the same chat, `bus_trip` resolved a street address in one town to a stop
named after the same street in a town about 150 km away. The Transitous
geocoder in `internal/connectors/bus/transitous.go` prefers any Dutch stop
over an address, and does not check the town the query names.

## Goal

- Every chat turn, background task and scheduled model turn sees the
  principal's saved routes and enabled watches in its system prompt, read
  from their stores at turn start. A question about a trip a route covers is
  answered with `route_check`.
- The tool hint knows the names and weekdays of the user's routes and can
  suggest `route_check`.
- `bus_trip` refuses a place in another town than the one the query names,
  instead of planning a trip to it.
- An eval case covers the failure and passes with the change.

## Decisions

- One section per store: `routes` and `watch` each get a `PromptSection` over
  their repository, like `schedule.PromptSection`. `turn.Jobs` becomes
  `turn.Section` (same method) and `Config.Jobs` becomes `Config.Sections`, a
  list joined in a fixed order: jobs, routes, watches. No new runtime request
  field: the joined text goes where the jobs text goes today, after memory.
  No schema change.
- Chat runs and background tasks get all three sections. Scheduled model
  turns (`RunScheduled`) get the routes and watches sections, chosen by the
  operator, and still no jobs section. Agent-watch checks, confined runs and
  the toolless runs get none.
- Entries are in creation order, and each section sorts explicitly by
  creation time and then id, as the jobs section does, instead of relying on
  the store's order. An edit keeps an entry in its place. Entry text holds
  only what the user defined: a route's name, weekdays, arrival target and
  leg outline (`Spec.OutlineFor`), and a watch's label and definition
  (`watch.Summary`), which hold no last-fired time, state or next run. So the
  cached prompt prefix changes only when a route or watch is added, changed,
  paused or removed.
- Caps: 300 runes per entry and 4000 per section, with a closing
  "N more not listed; route_list shows them." in the routes section and
  "…; watch_list shows them." in the watches section. Only enabled watches
  are listed. Text is English, like the rest of the prompt.
- The routes section heading tells the model to call `route_check` with a
  route's name when a question about getting somewhere matches its
  destination or weekday, before planning the trip with other tools. The
  existing route guidance in `internal/routes/tools.go` stays.
- The tool hint gets the route names and weekdays, passed through the same
  `Redact` as the message, and only when `route_check` is among the offered
  tools. Legs, stations and departures never go. The operator chose names
  over weekdays alone, knowing that `Redact` does not mask place names, so
  labels that name where the user goes reach TypeSafe. The route lister comes
  in through a small interface that `judgment` defines for extra state, wired
  in `wiring.go`, so `judgment` does not import `routes`.
- A `bus_trip` query with comma-separated parts accepts a geocoder match only
  when one of the parts, compared with `transit.Normalize`, equals one of the
  match's municipality or locality areas (`adminLevel` 8 to 10, which the
  recorded Transitous response uses for the gemeente and the woonplaats).
  Country, province, district and neighbourhood areas never count, so neither
  a trailing "Netherlands" nor a generic district name such as "Centrum"
  lets a match in another town through. A stop match is also accepted when
  the town before the first comma of its own name equals a part, so a stop
  named for a town but lying just outside its boundary still resolves; a
  wrong-town stop's name starts with the other town. A match with no areas is
  accepted only through that stop-name rule. A leading Dutch postcode is
  stripped from a part before comparing, and a part that is only a postcode is
  ignored. This covers both the stop convention ("Town, Stop") and the
  address form ("Street 15, Town"). When no match is accepted, the place is
  not found and the reply names the query and asks for the town as its own
  part. A query without a comma keeps today's preference order. The `to` and
  `from` descriptions ask for an address as "street and number, town".
- The turn's eight-call budget stays. With `route_check`, a day of trips takes
  one call per route.

## Approach

1. Add `turn.Section`, replace `Config.Jobs` with `Config.Sections`, and join
   the sections in the engine for chat runs and background tasks. In
   `runUnwatched`, add the routes and watches sections for scheduled turns
   only. Wire jobs, routes and watches in that order in `wiring.go`.
2. Add `routes.PromptSection` and `watch.PromptSection` over their
   repositories, each logging and leaving its section out on a read error so
   the turn still runs, like the jobs section.
3. Give `judgment.ToolHinter` an extra-state provider. When the user has
   routes and `route_check` is offered, add their redacted names and weekdays
   to the question state, and add one sentence to the question saying that a
   message about travelling where or when one of them goes needs
   `route_check`. Name the new data in the tool hint part of
   `docs/architecture.md`.
4. Read the geocoder's `areas` in `bus`, apply the town rule, and update the
   two place descriptions. Let the bus simulator seed areas on a place, in the
   shape of the recorded Transitous response in
   `internal/connectors/bus/testdata/transitous_geocode.json`.
5. Add an eval case with neutral places: seeded routes for two weekdays (the
   case world already seeds `routes`, `calendar` and `trains`), calendar
   events for the evenings and an all-day office event, and simulated trains.
   The user asks how to get to work and to the evening events on both days.
6. Update the "Scheduled jobs in conversation" part of
   `docs/architecture.md` to name the routes and watches sections and which
   runs get them.

Commits, one change each: the bus geocoder fix; the routes and watches
sections; the eval case; route names in the tool hint. The plan goes with the
last of them, done.

## Files affected

- `internal/turn/types.go`, `internal/turn/engine.go`, `internal/turn/task.go`,
  `internal/turn/scheduled.go`
- `internal/schedule/prompt.go` (interface assertions)
- `internal/routes/prompt.go` (new), `internal/watch/prompt.go` (new)
- `internal/wiring/wiring.go`
- `internal/judgment/hint.go`
- `internal/connectors/bus/transitous.go`, `internal/connectors/bus/tools.go`,
  `internal/testutil/bussim/simulator.go`
- Tests in the packages' existing test files, and `internal/eval/variant_test.go`
  if the rename reaches it
- `evals/cases/` (one new case)
- `docs/architecture.md`

## Execution

| Chunk | Task | Tier | Wave |
| --- | --- | --- | --- |
| A | Steps 1, 2 and 6: sections, wiring, docs | Complex, Opus (Jev complex 0.89) | 1 |
| C | Step 4: geocoder town rule, descriptions, tests | Complex, Opus (Jev complex 0.68, below threshold; the part and area matching has edge cases) | 1 |
| D | Step 5: eval case | Simple, Sonnet (Jev simple 0.91) | 1 |
| B | Step 3: route names in the tool hint | Complex, Opus (Jev simple 0.72, overridden: it widens what leaves for an external service) | 2, after A, since both edit `wiring.go` |

C and D share no files with A. Once D exists, the new case runs live against
a build of the base commit without A and B, and again after wave 2.

## Validation

- Sections, integration tests with a real temporary SQLite store:
  - A chat turn's, a background task's and a scheduled turn's system prompt
    list the principal's saved route and enabled watch. A scheduled turn has
    no jobs section.
  - A disabled watch is left out. A principal without routes or watches gets
    no such section. Another principal's routes and watches never appear.
  - Two turns with unchanged stores give byte-identical sections. Editing a
    route keeps its place in the list.
  - Over the cap, the section ends with the "more not listed" line naming the
    right listing tool.
  - A failing read leaves that section out and the turn still answers.
- Hint, test with a fake TypeSafe server: the question state holds the
  redacted route names and weekdays when the user has routes and
  `route_check` is offered, and holds none otherwise. No leg, station or
  departure from the seeded routes appears in the request.
- Bus, regression tests against the simulator. The first fails without the
  fix:
  - An address query naming one town, with a same-named stop seeded in
    another town, finds no place, and the reply names the query.
  - A match in the named town is chosen.
  - "Town, Stop" still resolves.
  - A trailing country does not let the other town's match through.
  - A district name shared with a match in another town does not let it
    through.
  - A stop named for the town but outside its areas still resolves.
  - A postcode part is ignored, and a postcode before the town is stripped.
  - A query without a comma behaves as today.
- Eval case:
  - It loads, and the CI check that cases name only offered tools passes.
  - It expects at least one `route_check` call, no `ns_trips` call, and reply
    times taken from the routes. Its world has no buses, so `bus_trip` is not
    offered.
  - The reply must not say a trip was left unchecked.
  - Live runs before and after are reported in chat.
- `gofmt`, module, test (with and without `-race`), vet, build and
  `scripts/check-plans`.

## Risks and open questions

- The prompt grows by the size of the sections, about 30 tokens per route and
  per watch for typical users, and 2000 at most for the two caps.
- A watch's summary may name stops and lines. It only goes into the
  principal's own prompt, as their jobs already do.
- A town the geocoder knows only by another name than the query's, such as a
  municipality name instead of the village, is not found. The model can
  retry with the municipality or a stop name.
- Plan 0170 replaces emails, phone numbers and addresses with placeholders in
  every model-bound `RunRequest` field, including `Jobs`. The new sections
  ride in that same field, so they are covered without a new field. Whichever
  plan lands second extends `internal/telegram/placeholders_integration_test.go`
  to assert that a route and a watch reach the model tokenized. The tool
  hint's TypeSafe state is outside that decorator.
- Scheduled turns that already spell out a route in their prompt now see the
  route twice. That costs tokens and does not change their instructions. The
  routes heading may also lead a scheduled job that mentions travel to call
  `route_check`, spending its tool calls.
- A comma query whose only extra part is a qualifier, such as "Station X,
  north side", no longer resolves. The reply asks for the town instead.
- A comma-less address such as "Street 15 Town" keeps today's order and can
  still resolve to a stop in another town. Only the tool description guards
  against it.
