# 0033 — Bus information: trips, stop status, and disruptions

Status: `done`

## Context

A typical trip is a bus between Amersfoort Stadhuis and Amersfoort
Centraal Station, and sometimes another bus ride. The NS connector from
[0027](0027-ns-train-information.md) covers trains only, and web search cannot
give live bus delays.

Researched on 2026-10-02:

- **The local operator has no public API of its own.** Every Dutch operator delivers
  live data to the national NDOV feed.
- **9292 is not a free option.** Its Reisadvies API is a paid business
  licence arranged through its sales desk, and scraping 9292.nl would break
  its terms.
- **OVapi (`v0.ovapi.nl`) serves the NDOV data free, without a key.** It is
  plain HTTP only: HTTPS did not connect.
  - `GET /tpc/<code>[,<code>…]` returns, for each timing point (one
    platform), its `Stop` and its `Passes`, keyed by journey (for example
    `<OperatorCode>_<date>_<line>_<journey>_<fortify>`). Each pass has `LinePublicNumber`,
    `DestinationName50`, `OperatorCode` (`CXX`), `TargetDepartureTime`,
    `ExpectedDepartureTime`, `TargetArrivalTime`, `ExpectedArrivalTime`,
    `UserStopOrderNumber`, `TripStopStatus` (`PLANNED`, `DRIVING`, `PASSED`
    seen; KV78 also defines `ARRIVED`, `OFFROUTE`, `CANCEL`, `UNKNOWN`),
    `TransportType`, and `WheelChairAccessible`. Times are local, without an
    offset.
  - The passes look about 90 minutes ahead. There is no live data beyond
    that.
  - `GET /stopareacode` (643 KB, 4,446 areas) is incomplete: some
    station platforms belong to no stop area. It is
    not used.
  - `GET /tpc` lists only codes and pass counts, without names.
  - `GET /journey/<OperatorCode>_<date>_<line>_<journey>_<fortify>` gives the
    whole trip, but it is not needed for a direct trip (see below).
- **Stop names come from OVapi's GTFS feed** (`gtfs.ovapi.nl/nl/gtfs-nl.zip`,
  253 MB, republished daily). The server supports range requests, so reading
  only the zip's central directory and the `stops.txt` entry costs about
  2 MB. `stops.txt` (5.6 MB unzipped) has `stop_code` (the timing point code
  OVapi uses), `stop_name` ("Amersfoort, Stadhuis"), `platform_code`,
  coordinates, and `parent_station`, which groups the platforms of one stop.
  Amersfoort Centraal Station has several platforms and Amersfoort Stadhuis a few.
- **A direct trip needs one call.** Asking `/tpc` for the platforms of both
  stops and joining on the journey key, keeping journeys whose destination
  `UserStopOrderNumber` is higher than the origin's, gave trips in both directions
  in 0.2 s. It finds lines whose sign names a
  different place (for example a line signed to a neighbouring town).
- **Disruption notices come from `gtfs.ovapi.nl/nl/alerts.pb`** (GTFS-realtime,
  about 390 KB, no key). On 2026-10-02 it held 667 alerts. Each alert has an
  informed entity naming a GTFS `stop_id` (8,268 references) and/or a
  `route_id` (4,381), a cause, an effect, active periods, and a header and
  description that are mostly Dutch, sometimes with English appended. 81% of
  the stop references matched the day's `stops.txt`. The rest point at stops
  missing from that day's feed.
- **NS cannot plan bus legs for this key.** `v3/trips` with coordinates
  returned `400` `API_KEY_NOT_ALLOWED_TO_PLAN_DOOR_TO_DOOR`.
- **Transitous (`api.transitous.org`) plans trips at any time, with
  changes.** It is a free, community-run MOTIS planner over the same open data
  plus OpenStreetMap.
  - `GET /api/v5/plan?fromPlace=<lat,lon>&toPlace=<lat,lon>&time=<UTC>&numItineraries=N`
    planned tomorrow 08:00 Amersfoort Stadhuis → Amersfoort Centraal Station as four
    direct buses.
  - It planned now → Leiden Centraal as a bus to Amersfoort Centraal Station plus an
    NS Sprinter, with `realTime`, `scheduledStartTime`, and `track` per leg.
  - Itineraries carry `startTime`, `duration`, and `transfers`. Legs carry
    `mode` (`WALK`, `BUS`, `REGIONAL_RAIL`, …), `routeShortName`,
    `agencyName`, `from`/`to` names, and times.
  - `GET /api/v1/geocode?text=…` returns stops (`STOP`) and places (`PLACE`)
    with coordinates.
  - Its terms (transitous.org/api) require a `User-Agent` with the
    application name, version, and a contact. They require a visible link to
    `https://transitous.org/sources/` and OpenStreetMap attribution. They allow
    only non-commercial use, ask that projects be open source and light on
    resources, and give no availability promise.

The configured home stop is set in `.env` as `HOME_AGENT_BUS_HOME_STOP` (a
timing point code of Amersfoort Stadhuis).

## Goal

Where the operator has enabled buses, a user can ask in plain language, or in
a scheduled prompt, "when is my next bus to the station", "bus from the
station home", or "what's going on at Amersfoort Centraal Station". The answer is live:
the next direct buses with line, departure, arrival, delay, and cancellations,
or a stop's departure board, with the operators' disruption notices. A trip at
a later time, by an arrival deadline, or with changes is planned, trains
included. Nothing changes where buses are off.

## Decisions

- **Two read tools in a new `internal/connectors/bus` provider,** through
  the broker as with NS: principal binding, deadline, size limit, the
  untrusted-data prefix, audit, and availability in scheduled turns.
  - `bus_trip(to, from?, date_time?, arrive_by?, limit?)` returns trips
    between two places. `from` defaults to the home stop. Default 5, at most
    10. It picks its source:
    - **Live direct buses (OVapi)** when `date_time` is absent or within the
      next 60 minutes, `arrive_by` is false, and a direct bus exists. Each
      trip gives line, operator, destination sign, platform, departure
      (planned, expected, delay in minutes), arrival at `to` (expected), and
      status.
    - **Planned itineraries (Transitous)** otherwise: a later time, an
      arrival deadline, or no direct bus. Each itinerary gives departure,
      arrival, duration, transfers, and its legs (mode, line, operator, from,
      to, times, track, delay where Transitous has live data, and walking
      minutes). Trains appear as legs, so a door-to-door question gets one
      answer.
    The answer names its source and says whether the times are live.
  - `bus_stop_status(stop?, limit?)` returns the departure board of a stop,
    defaulting to home: each departure's line, destination, platform,
    planned and expected time, delay, and status. Default 15, at most 30.
    A short summary on top counts delayed and cancelled departures, so "is
    anything wrong" has a direct answer.
  - Both tools append the disruption notices for the stops and lines in the
    answer: effect, header, description (trimmed to 500 characters), and the
    end of the active period. `bus_stop_status` lists every notice for the
    stop. A planned trip covers its two ends, the stops where the traveller
    boards, and its lines, and its notices are those active at the trip's
    time; a live answer uses now. A notice naming a line at one stop matches
    only when that stop is in the answer. At most 8 notices per answer, with
    those about a stop first.
- **Disruptions in this plan, not a follow-up.** `alerts.pb` is fetched on
  demand and cached for 2 minutes. It is decoded with `protowire` from
  `google.golang.org/protobuf`, which is already in the module graph and is
  promoted to a direct dependency. Only the alert fields used are read, so no
  generated GTFS-realtime bindings are added. An alert is active when one of
  its periods covers now (or it has none). It matches a stop through
  `stop_id`, and a line through `route_id`, using `routes.txt`, which is
  range-read with `stops.txt`. Text is passed through in its original
  language with the untrusted-data prefix, and the model translates. A failed
  alerts fetch does not fail the tool; the answer says that notices are
  unavailable.
- **Stops are resolved in Go from GTFS `stops.txt`.** It is fetched at most
  once a day through range reads on the zip and kept in memory: about 57,500
  platforms grouped by their exact `stop_name`, which is how travellers name
  a stop even where the feed splits it across stop areas. A failed refresh
  keeps the stale list, and the first fetch runs under the call's own
  deadline. A name matches the same way NS stations do: exact on timing point
  code or name, ignoring case, accents, and the repeated town ("Town,
  Town Station" also matches "Town Station"), then prefix and
  containment. Exact matches within 500 m of each other are one place, such
  as the railway platforms "Leiden Centraal" and the bus stop "Leiden, Leiden
  Centraal"; the one with most OVapi codes wins. Any other ambiguous name
  returns the candidates instead of guessing.
- **OVapi covers only part of the country.** GTFS gives a timing point code
  for only 18,123 of 57,541 platforms, and only 9,689 of those are known to
  OVapi. Coverage is good where the local operator and the Utrecht
  concessions run (around Amersfoort) and
  absent in Leiden, Den Haag, most of Amsterdam, and Rotterdam. A stop
  without codes, or one whose codes OVapi does not answer for, therefore gets
  its board from Transitous `v5/stoptimes` and its trips from Transitous
  planning, both of which carry live times where the operator publishes
  them.
- **OVapi for live, Transitous for planning.** OVapi is faster and states
  delays per stop, but it sees only about 90 minutes ahead and cannot
  combine rides. Transitous covers any time and transfers. The national
  schedule (`stop_times.txt`, 1.2 GB unzipped) is not loaded.
- **Places for Transitous.** A name that resolves in the stop directory is
  sent as the centroid of that stop's platforms. Otherwise it goes to
  Transitous geocoding, preferring a `STOP` in the Netherlands. An ambiguous
  result returns candidates as with stops. `date_time` is local
  Europe/Amsterdam time, sent as UTC.
- **Transitous terms, accepted by the operator.** Use is private and
  non-commercial and makes a few requests a day. The repository is private,
  not open source; the operator chose to accept that. The client sends
  `User-Agent: HomeAgent/<version> (+<contact>)`, with the contact
  from `HOME_AGENT_BUS_CONTACT`. The contact is optional and only labels the
  requests: while it is unset, Transitous is still used, with the plain
  `HomeAgent/<version>` agent, and the operator adds a dedicated address
  later. The first deployment withheld Transitous without a contact, which
  turned planning off entirely; the operator wanted it on from the start.
  The `/settings` section, `/privacy`, and every Transitous answer link to
  `https://transitous.org/sources/` and credit OpenStreetMap contributors.
  Live direct buses are tried first whenever the time allows; an OVapi
  failure then falls through to Transitous, and a Transitous failure is one
  sentence.
- **Operator switch, per-user opt-out.** `HOME_AGENT_BUS_ENABLED` (default
  false) is the operator's opt-in. `HOME_AGENT_BUS_HOME_STOP` is a timing
  point code, checked against the directory at the first call rather than at
  startup, so a bad code is a sentence to the model, not a crash. A
  `/settings` section "Buses" is on by default and stored as an
  opt-out integration row, as in 0026 and 0027, so no migration is needed.
  The home stop is shared by the household for now.
- **A fixed-host client, as in 0027.** Three fixed hosts (`v0.ovapi.nl`,
  `gtfs.ovapi.nl`, and `api.transitous.org` over HTTPS), redirects refused, no proxy, a 15-second timeout, a
  4 MiB limit on OVapi bodies and on `alerts.pb`, and an 8 MiB limit on each
  zip entry read.
  Plain HTTP is accepted: the data is public, nothing is authenticated, and a
  request reveals only stop codes. Responses are still treated as untrusted.
  The client sends a descriptive `User-Agent`.
- **Caching.** `/tpc` answers are cached for 20 seconds by code set, which
  absorbs repeated calls in a turn. Transitous plans are cached for 60
  seconds by request, its departure boards for 20 seconds, and geocoding
  results for a day. There is no background polling.
- **Guidance** tells the model:
  - use `bus_trip` for "when is my bus", the trip from home, and any trip
    that starts or ends with a bus, never web search;
  - prefer `ns_trips` for a train-only journey between stations, which has
    NS crowding and disruption detail;
  - use `bus_stop_status` for "what's happening at a stop";
  - give the first good option and the next one, with the delay;
  - say whether the times are live or planned.

## Approach

1. Spike: save trimmed fixtures from a live `/tpc` call for both Amersfoort
   stops, a small `stops.txt` and `routes.txt` sample, and an `alerts.pb`
   with a few alerts, plus Transitous plan responses for a direct, a
   transfer, an arrive-by, and a tomorrow trip, and a geocode response. Look
   for a `CANCEL` status in the wild, record the real pass horizon at
   different times of day, and check that `arriveBy` and `maxTransfers`
   behave as documented.
2. `internal/connectors/bus`:
   - the client and the range-read `stops.txt`/`routes.txt` directory;
   - the alerts decoder and its stop and line matching;
   - the Transitous plan and geocode client and the source choice;
   - the two tools, the journey-key join, and their text rendering;
   - `Status`/`SetEnabled` over the integration repository;
   - an HTTP simulator serving the fixtures and a small zip for tests.
3. Wiring: config (`internal/config/bus.go`), app registration, and the
   `/settings` section with its `/privacy` lines ("stops you ask about are
   sent to OVapi over plain HTTP"; "trip places and times are sent to
   Transitous, a volunteer-run planner") and the attribution.
4. Docs: `README.md`, `docs/architecture.md`, `docs/operations.md`,
   `.env.example`, `deploy/nas.env.example`, and this plan's results.

## Spike results

2026-10-02, against the live services:

- **OVapi.** `/tpc` for the platforms of Amersfoort Stadhuis and Amersfoort
  Centraal Station answered in about 0.2 s; the saved fixture has passes from
  11:21 to 12:53 for a call at 11:28, so the horizon was about 85 minutes.
  `TripStopStatus` values seen were `PLANNED`, `DRIVING`, `ARRIVED`, and
  `PASSED`; the spike saw no `CANCEL`, so cancellation is tested with an edited
  fixture. `JourneyStopType` marks `FIRST` and `LAST` calls.
- **GTFS by range.** The archive was 253 MB; reading `stops.txt` and
  `routes.txt` cost about 2 MB in a few 1 MiB range requests, and the whole
  first call, including the directory, took under a second.
- **Identifiers line up.** Transitous names Dutch stops and routes as
  `nl-OpenOV_<GTFS id>` (`nl-OpenOV_3894593`, `nl-OpenOV_147347`), the same
  identifiers the alert feed uses, so planned legs are matched to notices.
  Its stop areas are `nl-OpenOV_stoparea:<n>`.
- **Transitous.** `arriveBy=true` returned itineraries arriving before the
  time. `numItineraries` is a minimum, not a limit (asking for 1 returned 4),
  so results are cut to `limit`. `v5/stoptimes?stopId=…&n=…` returns a
  stop's departures, including its sibling platforms, with `realTime`,
  `cancelled`, `tripCancelled`, and tracks. Times are UTC.
- **Live run.** Through a real broker against all three services: both
  trip directions answered live from OVapi with delays and platforms;
  tomorrow 08:00 and an arrive-by trip to the Rijksmuseum were planned by
  Transitous with bus, train, tram, and metro legs; the first run showed
  ambiguous "Leiden Centraal", too many itineraries, and notices from every
  stop along a long trip, which led to the same-place, limit, and boarding
  stop decisions above.
- **Not checked.** `maxTransfers` is not used.

## Files affected

- new `internal/connectors/bus/` (OVapi, GTFS, alert, and Transitous clients, stops, tools, render, service,
  tests, and fixtures)
- `go.mod` (`google.golang.org/protobuf` becomes direct)
- new `internal/config/bus.go`, `internal/config/config.go`,
  `internal/app/app.go`
- `internal/telegram/settings.go`, `commands.go`, a new
  `internal/telegram/bus_integration_test.go`
- `.env.example`, `deploy/nas.env.example`, `deploy/compose.app.yaml` (no
  overlay: nothing is secret)
- `README.md`, `docs/architecture.md`, `docs/operations.md`,
  `docs/plans/README.md` (the numbered docs were consolidated by 0031)

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `go test -race ./...`,
  `scripts/check-plans`.
- Against a simulator:
  - the directory read from a zip by range, and fetched once a day;
  - an exact, an accented, a town-prefixed, and an ambiguous stop name;
  - a direct trip in each direction, including a line whose sign names
    another place;
  - a pass already `PASSED` at the origin is skipped;
  - a cancelled pass, a delayed one, and one without expected times;
  - no direct bus, and a time beyond the live window;
  - a 5xx and a timeout each turned into one sentence;
  - the 20-second cache;
  - an active, an expired, and a future alert, matched by stop and by line;
  - an alerts fetch failure leaving the departures intact;
  - source choice: now with a direct bus → OVapi, tomorrow → Transitous,
    `arrive_by` → Transitous, now without a direct bus → Transitous;
  - a Transitous itinerary with a transfer and a walking leg rendered;
  - a Transitous failure falling back to OVapi within the live window;
  - every Transitous request carrying the `User-Agent` with the contact;
  - no contact configured: Transitous still plans, without a contact in
    the `User-Agent`.
- Telegram integration: with buses off there is no section and no tools; on,
  they are on by default; one user's opt-out does not affect the other.
- Live, from the operator's chat: the morning trip to the station, the trip
  home, tomorrow's 08:00 trip, an arrive-by question, a stop status at
  Amersfoort Centraal Station, and a bus-plus-train trip.

## Risks and open questions

- OVapi is a free community service with no stated quota or availability
  promise. If it fails often, the fallback is reading NDOV GTFS-realtime
  `tripUpdates.pb` against the GTFS schedule, a much bigger job.
- Transitous is volunteer-run, may refuse non-open-source users, and may
  stop serving any client at any time. Live direct buses keep working
  without it. A self-hosted MOTIS or OpenTripPlanner on the NAS is the
  fallback if that happens.
- About a fifth of the alert stop references do not resolve against the
  day's `stops.txt`. Line matching through `route_id` covers some of these.
  The rest are dropped and counted in a log line.
- The `stops.txt` entry position changes with each daily feed, so the range
  reader always starts from the central directory, never from a cached
  offset.
