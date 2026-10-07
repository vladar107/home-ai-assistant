# 0056 — Answers grounded in tool data

Status: `proposed`

## Context

The audit behind plan 0055 found that the most damaging failures are
answers that look checked but are not:

- **Statuses nobody checked.** Morning reports printed "🟢 on time" for legs
  that had not been queried in that run. One run made no NS call at all and
  still showed two green trains. Others marked a leg green while NS returned
  a cancellation. The prompt asks for a status per leg, so the model fills
  the slot.
- **Times not in any result.** Chat answers quoted departures that appear in
  no tool output, or reused times from an earlier query after the current
  call had failed. The alternative suggested for the same cancelled train
  changed from one run to the next.
- **No way to ask about a specific train.** The user takes one of a few
  fixed departures. `ns_trips` only searches from a time (or arrives by
  one), so the model gets the earliest options and picks among them. It kept
  choosing an earlier train than the ones the user named, or a feeder train
  that matched the requested time.
- **Inconsistent units.** The user wants wind in knots. The model converts
  by hand in some runs and not in others, and sometimes adds "the card shows
  km/h" as commentary.
- **Invented facts about data and the app itself.** Without reading the
  table, the model guessed the next row of a Notion table ("next in the
  series is ..."). It assumed an order for a bulk Notion date update that was
  not the table's view order, and seven writes had to be undone. When asked
  how to connect an integration, it described a settings flow that does not
  exist.
- **Unclear refusals.** When web search is off after outside content, a
  deliberate rule kept as is, the model sometimes says it cannot search and
  sometimes that it can. It wanders through other tools instead of explaining
  the reason and offering `/new`.

## Goal

- Every train and bus status in a reply comes from a tool result in the same
  turn. A check that failed is reported as failed, never as "on time" or
  "no disruptions".
- The user can ask about specific planned departures and get one normalized
  status line per departure, the same way every time.
- Wind appears in the unit the user asked for, converted by the tool.
- The model does not claim an order, row, or setup step it has not read.

## Decisions

- **`ns_trips` gains `departures`.** It takes up to six planned departure
  times (`HH:MM`) on the date of `date_time`. The tool plans from each time,
  keeps the trip whose first train leg departs at exactly that planned time,
  and renders one line per departure. Each line has a normalized status: `on
  time`, `delayed +N min`, `cancelled`, `partly cancelled`, `changed
  platform N`, or `not in the timetable`. Then come the legs. A departure
  that is not found says so; nearby trains are never substituted.
- **The bus tools get the same treatment.** `bus_stop_status` takes an
  optional line and a planned departure time and reports that one departure
  with the same status words.
- **The status words are copied word for word.** The NS and bus guidance
  says to copy the status phrase unchanged. Trip renderers put it first on
  the line.
- **Run rules for scheduled turns.** The scheduled-run system preamble
  says: never show a status, time, or count that no tool returned in this
  run; if a check failed, say which one; leave a leg out rather than guess.
  The chat preamble gets the same rule for times and statuses.
- **Wind units are a tool argument.** `weather_forecast` and `weather_card`
  take `wind_unit` (`kmh`, `ms`, `kn`, `bft`). The model passes the
  preference from memory, and the tool converts. The card prints the unit
  it used.
- **Notion order.** Query results state their order: as sorted by the
  request, or "API order, not the view's order" when unsorted. Proposals
  that write to several rows name each row and the value being written, so
  the bundle shows the mapping. The guidance says to sort by a real property
  or ask the user before writing anything that depends on order.
- **`app_help` tool.** A read-only tool returns this app's own instructions
  for a topic: connecting each integration, `/settings` pages, `/jobs`,
  `/memory`, and why web search is off. The help comes from the same strings
  the settings pages render, so it cannot drift. Questions about setting up
  this app use it instead of answers from general knowledge.
- **The web search lock stays.** Its guidance and the `app_help` entry tell
  the model to say once, plainly, that search is off in this conversation
  because it has read outside content, and to offer `/new`.

## Approach

1. NS: the `departures` argument, exact-departure matching, the normalized
   status, and the renderer and guidance changes.
2. Bus: a single-departure status in `bus_stop_status`.
3. Weather: the `wind_unit` argument in the forecast, the card, and their
   renderers.
4. Notion: order labels on query results and per-row summaries for writes.
5. `app_help`: a small provider exposing help topics built from the
   settings strings.
6. The scheduled-run and chat preambles get the grounding rules. Tool
   guidance for status wording and the search lock.
7. README and `docs/architecture.md` describe `app_help` and the departure
   checks.

## Files affected

- `internal/connectors/ns/tools.go`, `service.go`, `render.go`, `status.go`
- `internal/connectors/bus` (stop status)
- `internal/connectors/web/weather.go`, `card.go`, `tools.go`
- `internal/connectors/notion/render.go`, `tools.go`
- a new help provider (location decided during implementation, next to
  `internal/telegram/settings.go` strings)
- `internal/runtime/pi` system preamble sections, `internal/conversation`
- `README.md`, `docs/architecture.md`

## Validation

- Integration tests with recorded NS and OVapi responses:
  - an exact departure that is on time, delayed, cancelled, or missing;
  - a feeder train never substituted for the requested departure;
  - wind converted to each unit;
  - Notion order labels;
  - `app_help` topics matching the settings pages.
- `go test -race ./...`, `go vet ./...`, formatting, build, and
  `scripts/check-plans`.
- Live check: a scheduled test run for a route with two fixed departures on
  a day with a known disruption; each status matches the NS app.

## Risks and open questions

- Prompt rules lower invented statuses but cannot prevent them. Plan 0057's
  route check moves status rendering fully into code for saved routes. This
  plan covers ad-hoc questions.
- Matching "exactly that planned departure" depends on NS returning the
  trip when planning from that minute. Implementation should confirm this
  with recorded responses, and fall back to the departures board for the
  first leg if needed.
