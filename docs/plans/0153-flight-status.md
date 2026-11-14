# 0153 — Flight status: delays, gates, and cancellations for a trip's flights

Status: `proposed`

## Context

A trip's flight is a booking of kind `flight` with a title, a booking reference, and its
departure instant. Plan 0139 checks a trip near or under way about twice a day for news in mail,
on the web, and in the calendar, and plan 0146 made those updates short and in the language of
the trip's tab. Nothing checks the flight itself. A delay, a gate change, or a cancellation
reaches the user only if the airline mails about it and the next check, up to eight hours later,
finds the mail. The user cannot ask "is EX123 delayed?" either: the model can only guess from a
web search.

A booking also does not hold the flight's number. The scan and `trip_add` write it into the
title at best, so code cannot look a flight up.

Flight status is sold by a handful of providers. Researched on 2026-11-14 from their public
pricing and documentation pages, nothing signed up for:

| Provider | Free or cheapest | Cost of one lookup | Fields for one flight | Auth |
| --- | --- | --- | --- | --- |
| [FlightAware AeroAPI](https://www.flightaware.com/commercial/aeroapi/) | Personal tier: no monthly minimum, the first $5 of usage a month free; personal or academic use only; 10 result sets a minute; no alerts | $0.005 a result set of up to 15 flights | scheduled, estimated, and actual gate and runway times, departure and arrival delay, gate and terminal at both ends, cancelled, diverted, status text, airports with their time zones; a day's window from 10 days back to 2 days ahead | `x-apikey` header |
| [AeroDataBox](https://aerodatabox.com/pricing/) (RapidAPI, API.Market) | Basic: 400 units a month, 1 request a second, no commercial use, attribution required; Pro $7.50–8 for 5,000 units | 2 units (a tier 2 endpoint) | scheduled, revised, predicted, and runway times, gate, terminal, check-in desk, baggage belt, a status of `Expected`, `CheckIn`, `Boarding`, `GateClosed`, `Departed`, `Delayed`, `Arrived`, `Canceled`, `Diverted`, and others | marketplace key header |
| [aviationstack](https://aviationstack.com/product) | Free: 100 requests a month, non-commercial, live flights only; Basic $49.99 a month | 1 request | times, delay, gate, terminal, status (not verified) | `access_key` query parameter |
| [AirLabs](https://airlabs.co/) | Free package with unstated limits; about $19 a month for 10,000 | 1 query | schedules with delay and status; gate and terminal not verified | `api_key` query parameter |
| [Flightradar24 API](https://www.flightradar24.com/blog/flightradar24-api/) | Credit plans from about $9 a month (not verified) | not published | flight summaries from ADS-B and schedules | not verified |
| [Cirium (FlightStats)](https://developer.flightstats.com/getting-started/pricing), OAG | Evaluation, then quote-based enterprise contracts | not published | full status, gates, delays, alerts | not verified |
| [OpenSky Network](https://openskynetwork.github.io/opensky-api/rest.html) | Free credits a day with an account | 4 credits | ADS-B positions only; flights by aircraft from the previous day on; no schedules, gates, or delays | OAuth client credentials |
| Airport and airline developer portals | Free | 1 call | one airport's or one airline's flights only | per portal |

OpenSky has no schedules and cannot answer "is it delayed". Airport and airline APIs cover one
airport or one airline each, while a household flies several. The enterprise feeds have no price
for a household. That leaves AeroAPI and AeroDataBox; aviationstack's free tier is too small and
its paid tier too dear.

## Goal

Where the operator has configured a flight status key and the user has Flight status on:

- the user can ask, in plain language or in a scheduled prompt, about any flight by its number
  and day, and the answer comes from the provider: times, delay, gate, terminal, and status;
- a tracked trip's flight is followed on its day of travel, and the trip's tab gets one short
  message when the flight is delayed, moves gate or terminal, is cancelled, or is diverted, and
  nothing when nothing changed;
- the household never spends more than a configured monthly number of lookups, and says so
  instead of silently going quiet when it runs out.

Success: a delayed test flight on its day gives one message naming the new and old departure
time, a gate change gives one line, an unchanged flight gives none, and the month's lookups stay
under the budget.

## Decisions

- **FlightAware AeroAPI is the provider, with no fallback.** Its Personal tier is licensed for
  personal use, which a private household assistant is, and its free $5 a month buys about a
  thousand lookups, five times AeroDataBox's free 400 units at 2 units each. Its flight object
  carries delay, gate, terminal, cancellation, and diversion as fields, not only as a status word,
  and names each airport's time zone. Two weaknesses are accepted: it has no alerts on the
  Personal tier, so the assistant polls, and usage over $5 is billed, so the budget below is
  enforced in code. A second provider at run time is not built: two keys, two privacy lines,
  and two shapes of data that disagree cost more than an hour of "could not check" on a rare
  outage. The provider sits behind an interface so AeroDataBox, whose free plan stops at its quota
  instead of billing, can replace it with one new implementation if the operator prefers it.
- **One operator key, a per-user switch.** A flight number and a date are public data and are
  not tied to a user's identity at the provider, as with the NS key in plan 0027. The household
  shares one key; each user has a `Flight status` switch in `/settings`, on by default, stored as
  an opt-out integration row. A per-user key is not offered: it would make each user open a
  billed FlightAware account for data that identifies no one. The `/privacy` line says that
  flight numbers and dates the user asks about or that their tracked trips hold are sent to
  FlightAware. A booking reference, a name, or a seat is never sent.
- **A typed connector, `internal/connectors/flights`.** It follows the NS connector:
  - a `Provider` interface with one call, `Flights(ctx, designator, from, to)`, that returns the
    normalised legs operating with that designator in the window;
  - the AeroAPI implementation: `GET /aeroapi/flights/{ident}?ident_type=designator&start&end&max_pages=1`
    on a fixed base, the key in the header and never in a URL or a log line, redirects refused,
    no proxy, a 15-second timeout, a 1 MiB body limit, and `httpretry` for transient failures.
    Only one page is ever requested, since each page is billed, and the window is clamped to the
    provider's horizon, 10 days back to 2 days ahead, before it is sent;
  - one rate limiter in the client, shared by the tool and the tracking, that spaces requests at
    least six seconds apart, so AeroAPI's ten a minute is never reached. It is a mutex and a time,
    with no new dependency;
  - a normalised `Leg`: designator, operating designator and codeshare designators, origin and destination (IATA code, name,
    time zone), scheduled, estimated, and actual departure and arrival, departure and arrival
    delay, gate and terminal at each end, cancelled, diverted with the airport diverted to, and
    the provider's status text;
  - error classes as NS has them, each one sentence safe for the model and the log: unauthorized,
    not found, rate limited, budget spent, unavailable, timeout, unexpected response.
- **A designator is checked and normalised before it is sent.** Uppercased with spaces removed,
  it is an airline code of two letters or digits, not both digits (`EX`, `U2`, `3U`), or of three
  letters, then one to four digits with an optional letter. Leading zeros of the number are
  removed, so `EX 0123` and `EX123` are one designator and one cache entry. Anything else is
  refused without a lookup.
- **A codeshare is the flight it is sold as.** AeroAPI may answer a marketing number with the
  operating flight. A leg whose operating designator differs from the one asked for is taken when
  the asked designator is among its codeshares, and messages name it as asked, with the operator's
  number once: `EX123 (operated as OP456)`. The cache key stays the asked designator.
- **The budget is a monthly count of requests, kept in SQLite and reserved before sending.** A
  transport under `httpretry` reserves each attempt, retries included, before it goes out, with one
  atomic upsert that increments the month's counters only while they are under their limits and
  returns whether it did. The counters are household-wide, keyed by calendar month in UTC: one for
  the tool, one for tracking, and one per user for the tool; the purpose and the user travel in
  the request's context. Concurrent turns and the tracking therefore cannot overrun the budget,
  and it survives restarts.
  - `HOME_AGENT_FLIGHTS_MONTHLY_LOOKUPS` defaults to 450: $2.25 at AeroAPI's price, so even a
    month whose count was lost with the database and spent twice stays at $4.50, under the free
    $5.
  - Questions asked through the tool may use at most a third of it, 150 by default, so a chatty
    month cannot starve the tracking of a flight; tracking may use all of it.
  - One user's questions may use at most half of the tool's third, so one user cannot spend the
    household's.
  - An attempt that cannot be reserved is not sent and returns the budget-spent error.
  - Results are cached for five minutes by designator and origin-local date, shared by the tool,
    every user's tracking, and every trip, so two household members on the same flight cost one
    lookup.
- **A booking holds its flight number.** `trip_bookings` gains an optional `flight_number`, only
  for kind `flight`, validated as above. The scan's reply and `trip_add`/`trip_update` gain the
  field, the scan's prompt asks for it from the confirmation mail, and `trip_list` shows it. A
  connecting journey is one booking per leg, as the scan already writes them. A booking without
  a number is not tracked. The field is in the export.
- **The on-demand tool is `flight_status(flight_number, date?)`.** It is read-only, so scheduled
  turns may call it too.
  - The date is the departure day local to the origin; it defaults to today in
    `HOME_AGENT_TIMEZONE`. The window sent is that day widened by 14 hours each side, so a
    departure in any time zone is found, and the legs whose scheduled departure falls on that
    date at their own origin are returned.
  - A date more than two days ahead, or more than ten days back, is answered without a lookup:
    the provider has no status that far out.
  - Each leg is one block: a status phrase first (`on time`, `delayed +N min`, `cancelled`,
    `diverted to …`, `departed`, `landed`, or `no live data yet`), then route, departure and
    arrival as scheduled and as now expected or actual, in each airport's local time with the
    airport code, gate and terminal when known, and a closing line naming FlightAware as the
    source.
  - The guidance says: use this tool, never web search, for a flight's status; copy the status
    phrase word for word; every time and gate must come from a result in this turn; a failed call
    is reported as a failed check, never as "on time". The result is outside content, as NS
    results are.
- **Tracking runs in the trips loop, without a model turn.** A new `Flights` part of the trip
  package, beside `Updates`, is driven by the loop's cycle. The trip package declares the small
  interface it needs; the wiring adapts the connector to it, as it does the calendar reader.
  - A flight booking is tracked while its trip is planned, its owner's Trips scan job is
    enabled (the gate plan 0139 set), and the owner has Flight status on, from 38 hours before the
    booking's stored departure until the matched leg ends. The 38 hours are the 24 of the cadence
    plus the 14 by which a stored departure can be off, as below.
  - **A booking's departure is not trusted as an instant.** `trip_add` and `trip_update` read
    times in the trip's zone, the destination's, so an outbound flight typed as its local 09:30
    is stored hours off; the scan writes instants with their offset and is usually right. The leg
    is therefore matched by designator and day, then by time: the lookup covers the booking's
    date in the trip's zone and the days either side, and the leg taken is the one whose
    scheduled departure either is the stored instant or reads, at its own origin, as the stored
    wall-clock time in the trip's zone, each within 30 minutes. When none matches and exactly one
    leg operates under the designator on that date at its origin, that one is taken. Otherwise
    the line "EX123 on Mon 9 Nov was not found; check the flight number and time on the trip" is
    sent once and the booking is looked up again only every six hours.
  - Once matched, the leg's own scheduled departure is D for the cadence, and its origin-local
    date is the date of every later lookup and of the cache key. The booking itself is not
    changed.
  - At most one lookup runs at a time, in its own goroutine, under the loop's context and joined
    at shutdown, so start and end notices are never held up.
  - Messages are sent after the lookup is done and outside the lookup goroutine. The 0146 rewrite
    is a model call on the trip owner's own subscription and can take seconds or fail; it never
    holds up the next lookup.
- **The cadence follows the departure, on slots shared by everyone following the leg.** Each
  phase's slots are fixed times counted from D, so every trip and user following one leg is due
  at the same slot and the second is served from the cache. With D the scheduled departure and A
  the expected arrival:
  - from D−24h to D−4h, every four hours, and not between 23:00 and 07:00 at the origin;
  - from D−4h to D, every 20 minutes, at any hour;
  - from D until landed, every hour, and once more at A+15 min;
  - nothing after landing, cancellation, or A+3h.

  That is about 20 lookups a leg, so the default budget follows about 15 legs a month after the
  tool's third. When the budget left this month would not cover the legs still ahead, the far
  phase is dropped first, then the in-flight phase; the 20-minute phase before departure is kept
  longest.

  Times are local to the airport they belong to. The message ends with `Source: FlightAware.`
  Whether something changed is decided by comparing with the stored last lookup, not by the sent
  log. Each change is then recorded as a `SentUpdate` with source `flight` and a reference of
  designator, date, field, new value, and the lookup's time, so a gate that goes B7, B12, B7, B12
  or a delay that comes back is reported each time, and the 0139 check's prompt lists what was
  sent. `trip_updates_sent` allows only `mail`, `web`, and `calendar` as a source, so its table is
  rebuilt to allow `flight`.
- **Failures are said once, not repeated.**
  - A transient failure is retried by `httpretry`; a lookup that still fails records nothing and
    the leg waits for its next turn in the cadence.
  - When no lookup for a leg has succeeded for an hour inside D−4h, the tab gets one line,
    "Flight status for EX123 could not be checked for an hour; check the airline's app.", once
    per leg.
  - When the budget is spent, tracking stops until the month turns, each user with a flight in
    the window gets one line naming the date it resumes, once a month, the tool answers with that
    sentence, and the operator gets a warning log line and a metric.
  - A refused key is logged as `unauthorized` and every lookup is skipped until restart, since
    each retry would fail alike; the tool says the operator needs to check the key.
- **State.** Migrations are numbered 058 to 060, fixed across the plans drafted beside this one,
  in chunk order: 058 the monthly count, 059 the booking's flight number, 060 the flight state
  and the rebuilt `trip_updates_sent`. A new table keeps, per trip and leg (designator and local date), the chosen
  provider flight ID, the last stored lookup as JSON, the last attempt, the last success, and
  whether the failure line and the not-found line were sent. It is deleted with its trip and left
  out of the export, as the 0139 update state is. The monthly count is its own table, also left
  out.
- **Operations.** `HOME_AGENT_FLIGHTS_API_KEY` or `HOME_AGENT_FLIGHTS_API_KEY_FILE` turns the
  connector on, as the NS key does; production reads it from a Keychain item that
  `scripts/nas-sync-secrets` materialises as a Docker secret, and `HOME_AGENT_FLIGHTS_ENABLED`
  adds a `deploy/compose.app.flights.yaml` overlay. Metrics: lookups by outcome and the month's
  count against the budget.
- **Not in this plan:** a flight row on the day card or a status widget, alerts pushed by a paid
  tier, departed and landed messages, seat or booking changes, and flights not on a trip being
  followed on their own.

## Approach

1. **Spike** by the operator once the key exists: `curl` `/flights/{ident}` for a flight on
   time, a delayed one, a cancelled one, and one by a marketing number, on flights no household
   member is on, save trimmed responses as test fixtures with neutral values, and record here the field names, the delay unit, how a diversion and a
   multi-leg designator look, and whether a one-day window is one result set. Anything that
   contradicts the decisions is amended here before building.
2. **Connector**: config, client, counting transport and budget store, cache, AeroAPI
   implementation, switch, the tool and its rendering, wiring, and the settings and privacy
   lines.
3. **Flight number on the booking**: migration, repository, scan reply and prompt, trip tools,
   export.
4. **Tracking**: state table, the `Flights` part with its cadence, leg matching, comparison,
   messages, failure lines, and the loop's hook.
5. **Documentation and operations.**

## Implementation chunks

1. **Spike and fixtures.** Simple. Depends on the operator's key. Files: this plan,
   `internal/connectors/flights/testdata/`. Accepted when the fixtures, taken from flights no
   household member is on, cover on time, delayed, cancelled, diverted, two legs under one
   designator, and a marketing number answered with its operating flight; and this plan names the
   confirmed fields and the rule that matches a codeshare to its operating designator.
2. **Connector client and budget.** Complex. Depends on 1. Files: new
   `internal/connectors/flights/` (client, provider, AeroAPI, legs, budget, cache, service),
   `internal/config/flights.go`, migration `058_flight_lookups.sql` and its repository in
   `internal/store/sqlite`. Accepted when a fake AeroAPI over `httptest` shows: the key only in
   the header; one page requested and the window clamped to the horizon; a malformed designator
   sends nothing, and `EX0123`, `U2123`, and `3U123` normalise as above; the cache serves a
   repeat; retried attempts are counted; concurrent requests at the limit reserve no more than
   it; the budget, the tool's third, and a user's half of it refuse without a request and survive
   a restart on the same database; requests are spaced by the limiter; each error class is one
   sentence without the key or the body.
3. **`flight_status` tool and wiring.** Complex. Depends on 2. Files:
   `internal/connectors/flights/tools.go` and rendering, `internal/wiring/wiring.go`,
   `internal/telegram/switch_pages.go` (the switch page, its help topic, and its settings entry),
   `internal/telegram` (privacy line), `internal/prompt` if the guidance needs it. Accepted when
   the tool is offered only with a key and the switch on, renders each status phrase, picks the
   legs of the local date across time zones, answers a date out of range without a lookup, and
   names a codeshare as asked, and says "check failed" when the fake provider fails.
4. **Flight number on bookings.** Simple. Independent of 2 and 3. Files:
   migration `059_trip_booking_flight_number.sql`, the trip repository, `internal/trip/trip.go`, `scan.go`,
   `service.go`, `tools.go`, `export.go`. Accepted when a scan reply and `trip_add` store a
   number, a malformed one is refused with a sentence, a non-flight booking cannot have one, a
   trip update keeps it, and an export and restore round-trip it.
5. **Tracking and change messages.** Complex. Depends on 2 and 4. Files:
   `internal/trip/flights.go` and its integration test, `internal/trip/loop.go`, migration
   `060_trip_flights.sql` (the flight state and `trip_updates_sent` rebuilt to allow `flight`)
   and its repository, `internal/wiring/trips.go`. Accepted when, on a temporary database with a
   fake AeroAPI, a fake clock stepped from slot to slot, and the Telegram simulator: the cadence
   makes the expected number of lookups from D−24h to landing; nothing is looked up outside the
   window, with the scan job off, or with the switch off; an outbound booking stored in the
   destination's zone and one stored as a true instant both match their leg; the baseline is
   silent for an on-time flight; a 45-minute delay, a gate change, a cancellation, a diversion,
   and a return to on time each give the line above once; a gate going B7, B12, B7, B12 gives
   three lines; a 10-minute wobble gives nothing; two users on the same flight, stepped to the
   same slot, cost one lookup; a rewrite that hangs delays no lookup; an hour of failures before departure gives one line; a spent budget gives one line per
   user and stops lookups until the month turns; shutdown during a lookup returns promptly; and
   the message reaches the trip's tab in its language through the 0146 rewrite.
6. **Documentation and operations.** Simple. Depends on 3 and 5. Files: `README.md` (trips,
   settings, privacy), `docs/architecture.md`, `docs/operations.md` (sign-up, key rotation,
   budget), `.env.example`, `deploy/nas.env.example`, `deploy/compose.app.flights.yaml`,
   `scripts/nas-sync-secrets`, `scripts/deploy-app-nas`, and the operator's local `.env` and
   `deploy/.env.nas`. Accepted when a fresh reader can configure the key from the docs alone.

## Files affected

- new `internal/connectors/flights/` with tests and fixtures
- `internal/config/` (new `flights.go`)
- `internal/store/sqlite/migrations/` (`058_flight_lookups.sql`, `059_trip_booking_flight_number.sql`,
  `060_trip_flights.sql`) and the trip and budget repositories
- `internal/trip/` (`trip.go`, `scan.go`, `service.go`, `tools.go`, `export.go`, `loop.go`, new
  `flights.go`)
- `internal/wiring/wiring.go`, `internal/wiring/trips.go`, `internal/wiring/export.go`
- `internal/telegram/switch_pages.go` and the privacy line in `internal/telegram`
- `README.md`, `docs/architecture.md`, `docs/operations.md`, `.env.example`,
  `deploy/nas.env.example`, `deploy/compose.app.flights.yaml`, `scripts/nas-sync-secrets`,
  `scripts/deploy-app-nas`

## Validation

- Formatting, module, vet, build, race and non-race tests, and `scripts/check-plans`.
- The tests named in the chunks. The provider is faked at its HTTP boundary with the spike's
  fixtures; the database is a real temporary SQLite file; no test reaches FlightAware.
- Live, by the operator after deployment: `flight_status` for a flight departing today, and a
  test trip holding a flight on its day, watching the trip's tab and the month's lookup count in
  Grafana.

## Risks and open questions

- **The key costs money past $5.** AeroAPI's Personal tier bills usage over its free $5, so a
  billing account is needed and the code's budget, with a portal spend cap if one exists,
  guards against a bill. A count lost with the database could let one month reach about twice
  the budget, still under $5 at the default.
- **Gates and terminals are sparse** outside large airports and appear late; a missing gate is
  not reported as a change.
- **Two days ahead is the provider's horizon,** so the tool cannot answer for next week; the
  scan's schedule changes from mail still come through the 0139 check.
- **A codeshare number** may be returned under the operating carrier's designator. The spike
  checks how AeroAPI answers a marketing number; the booking's number is what the airline mailed.
- **The cadence is a guess** at what a traveller wants; 20 minutes before departure can miss a
  gate change by that long. It is tuned on live trips.
- **Polling while the trip runs reads nothing private,** but it tells FlightAware which flights
  the household follows, from one key.
- **Open for the operator:**
  - open an AeroAPI Personal account with billing, or use AeroDataBox's free plan, which needs no
    card but buys a fifth of the lookups;
  - read AeroAPI's terms on storing and redistributing its data before signing up, since the
    assistant keeps the last lookup per leg and sends its content to a model and to Telegram;
  - set a spend cap in the AeroAPI portal if it offers one, as a second guard beside the code's
    budget;
  - the default budget of 450 lookups a month, the tool's third of it, and a user's half of
    that;
  - the 15-minute delay threshold;
  - whether "departed" and "landed" deserve a message in a later plan.
