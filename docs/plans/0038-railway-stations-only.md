# 0038 — Tell the model that NS tools take railway stations only

Status: `done`

## Context

Scheduled test runs of an evening trip on 2026-10-02 repeatedly called `ns_trips`, and the
`trains` rows of `weather_card`, with a bus stop as the departure station, such as
`Bushalte Stadhuis, Amersfoort`. The resolver rightly found no NS station. The model then
retried with `Amersfoort` and succeeded, but every run wasted a tool call, and one card summary told
the user the bus leg "could not be built". The same happened after the bus tools went live,
because nothing tells the model that NS tools know railway stations only.

## Goal

- The model is told up front that NS tools and the card's train rows take railway stations, and
  that a bus, tram or metro leg is planned separately.
- When a name still fails, the rejection names the railway station or stations that appear inside
  it, so the next call can succeed without guessing.
- The resolver stays strict. A stop name never silently becomes a station.

## Decisions

- **The suggestion is advice, not a match.** The model still has to call again with the station,
  so a wrong suggestion stays visible instead of planning a trip from the wrong place.
- **A station counts as named in the query** when one of its known forms occurs there as whole
  words. A station whose form lies inside a longer named form is dropped, so
  `Amsterdam Science Park` does not also suggest every station called Amsterdam. Dutch stations
  are preferred, as in normal matching.
- **The guidance names the bus tools only "when they are offered",** because each user turns
  buses and trains on separately.

## Approach

1. **Resolver** (`internal/connectors/ns/stations.go`):
   - When nothing matches, `resolveStation` collects the stations named inside the query.
   - `resolutionError` says NS tools only know railway stations, then either names those
     stations with their codes or asks the user.
   - The existing `No NS station matches <role> "<query>"` opening stays.
2. **Guidance** (`internal/connectors/ns/tools.go`): add one sentence on railway stations and on
   legs that start or end at a stop.
3. **Card schema** (`internal/connectors/web/tools.go`): the `from` and `to` descriptions of a
   `trains` row say "railway station".

## Files affected

- `internal/connectors/ns/stations.go`, `internal/connectors/ns/tools.go`
- `internal/connectors/web/tools.go`

## Validation

- `gofmt`, the module check, `go vet`, `go test ./...`, `go build`, `scripts/check-plans`.
- After deployment, a scheduled run of the same trip makes no `ns_trips` call that fails station
  resolution.

## Risks and open questions

- A stop name that contains no station name (`Bushalte Centrum`) still gets only the general
  advice. The model then has to ask the user or use the bus tools.
