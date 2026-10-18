# 0103 — Watches and routes on transit abstractions

Status: `done`

## Context

An architecture audit (finding F5) found the departure and disruption watches,
saved routes, and the web day card bound to the concrete train and bus
connectors:

- `watch.NewDepartureEvaluator` takes `*ns.Service` and `*bus.Service`, and
  `watch.NewDisruptionsEvaluator` takes `*ns.Service`. The evaluators build
  `ns.DepartureQuery` and `bus.DepartureQuery`, read `ns.Disruption`, and
  compare refusal texts with `ns.DisabledText` and `bus.DisabledText`.
- `routes` defines `Trains` and `Buses` interfaces, but their methods still take
  the connectors' own query types and refusals.
- `internal/app` carries an adapter, `nsTrains`, with two enum-mapping tables, so
  the web day card can ask NS for a trip status without importing it.
- `ns` and `bus` each have their own copy of the station or stop name
  normalisation (`normalize`, `accentFolds`), of the refusal error type, and of
  the directory loader that fetches a list once a day with concurrent callers
  sharing one fetch.

The leaf package `internal/transit` already holds the shared departure-check
vocabulary (`Clock`, `Status`, `Departure`, `Write`) but nothing above it uses
it as a boundary.

## Goal

Watches, routes, and the web day card depend only on `internal/transit`, so
another transit provider that implements its interfaces plugs into departure
watches and route legs without editing `watch` or `routes`. Both connectors
share one copy of the name normalisation and the directory loader. Behaviour
stays the same: the same replies, alerts, refusals, upstream-failure
classification, and fetch pattern.

## Decisions

- `internal/transit` stays a leaf package. It imports only the standard
  library, and the connectors implement its interfaces.
- One provider-neutral `transit.DepartureQuery`, with `From`, `To`, `Via`,
  `Line`, `Towards`, `Date`, and `Times`. Each service reads the fields it
  knows. A train service reads stations `From`, `To`, and `Via`. A bus service
  reads stop `From`, where empty means its home stop, plus `Line` and
  `Towards`. The connectors' own query types go away.
- `transit.Resolver` (`ResolvePlace(ctx, principal, role, query)`) and
  `transit.Checker` (`CheckDepartures`) combine into `transit.Departures`. The
  bus connector now passes the role into its refusal text. Watches and routes
  pass `"stop"`, as the old method did, so the text is unchanged.
- One `transit.RefusalError` replaces `ns.RefusalError`, `bus.RefusalError`,
  and `web.TrainRefusal`. A new `Disabled` flag marks the owner having switched
  the service off. Watches used to recognise that by comparing the text with
  each connector's `DisabledText`. Now they check the flag, so the upstream
  classification stays the same without knowing any provider's wording.
- `routes.LegResult` gains `Disabled`, set when a leg's service refused because
  the owner switched it off. Route watches use it, instead of the connectors'
  disabled texts, to tell "trains and buses are off" from an upstream failure.
- The trip-status vocabulary moves into `transit`: `TripQuery`, `TripStatus`,
  `Level`, `Cause`, and the `Trips` interface (`Enabled` and `TripStatus`).
  The NS connector implements it directly, with a new `Enabled` method. The
  web connector takes `transit.Trips` and maps `transit.Level` to the day
  card's pill colour itself. The `nsTrains` adapter and its mapping tables in
  `internal/app` go away.
- Disruptions move into `transit` as `Disruption` and `AffectedStation`, behind
  a `transit.Disruptions` interface (a `Resolver` plus `Disruptions`) that the
  disruption watch takes.
- `transit.Normalize` is the single name fold that both connectors use.
- `transit.Directory[T]` is the generic single-flight loader. It takes the
  fetch, lifetime, retry delay, and a mapping for the error a waiting caller
  gets when its own context ends. Each connector keeps its own lifetimes and
  error classes. The bus alert feed is a different, non-single-flight cache and
  stays as it is.
- Watch's upstream-failure telemetry still labels departure checks by mode,
  `ns` for trains and `bus` for buses. Renaming the labels would change the
  metrics.

## Approach

1. Add to `internal/transit`: the departure query and interfaces, the refusal
   error, trip status, disruptions, `Normalize`, and `Directory`.
2. Move `ns` and `bus` onto them. They use `transit.RefusalError` with
   `Disabled` set where they refuse for the switch, `transit.DepartureQuery`,
   `ResolvePlace`, `transit.Normalize`, and `transit.Directory`. `ns` also
   uses the transit trip-status and disruption types, and gains `Enabled`.
3. Change `watch` (departure, disruptions, route evaluators) and `routes`
   (config, leg checks, leg resolution) to take and use only `transit` types.
4. Change `web` to take `transit.Trips` and `transit.RefusalError`. Remove the
   adapter from `internal/app`, and wire the connectors in directly, taking
   care that a missing connector stays a nil interface.
5. Update the existing tests to the moved types and method names. Assert the
   `Disabled` flag on the connectors' switched-off refusals. Add a route-watch
   test that tells a switched-off owner apart from NS failing, since that
   classification moved from text comparison to the flag.

## Files affected

- `internal/transit/`
- `internal/connectors/ns/`, `internal/connectors/bus/`
- `internal/connectors/web/service.go`, `internal/connectors/web/card.go`,
  and their tests
- `internal/watch/departure.go`, `internal/watch/disruptions.go`,
  `internal/watch/route.go`, `internal/watch/transit_integration_test.go`
- `internal/routes/service.go`, `internal/routes/tools.go`
- `internal/app/app.go`

## Validation

- Formatting, module tidiness, vet, build, and the full race test suite.
- The existing NS, bus, watch, routes, and web integration tests pass unchanged
  in what they assert.

## Risks and open questions

- A new transit *mode*, such as trams, would still need watch and route changes
  for its target fields and tool schema. This plan only makes the provider
  behind the existing train and bus modes replaceable.
