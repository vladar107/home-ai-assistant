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
| [AeroDataBox](https://aerodatabox.com/pricing/) (RapidAPI, API.Market) | Basic, free on RapidAPI: 400 units a month, 1 request a second and 1,000 an hour, no commercial use, attribution required; Pro $7.50–8 for 5,000 units | 2 units (a tier 2 endpoint) | scheduled, revised, predicted, and runway times, gate, terminal, check-in desk, baggage belt, airports with their time zones, data quality markers, and a status of `Unknown`, `Expected`, `EnRoute`, `CheckIn`, `Boarding`, `GateClosed`, `Departed`, `Delayed`, `Approaching`, `Arrived`, `Canceled`, `Diverted`, or `CanceledUncertain` | marketplace key header |
| [FlightAware AeroAPI](https://www.flightaware.com/commercial/aeroapi/) | Personal tier: no monthly minimum, the first $5 of usage a month free, billed past it; personal or academic use only; 10 result sets a minute; no alerts | $0.005 a result set of up to 15 flights | scheduled, estimated, and actual gate and runway times, departure and arrival delay, gate and terminal at both ends, cancelled, diverted, status text, airports with their time zones; 10 days back to 2 days ahead | `x-apikey` header |
| [aviationstack](https://aviationstack.com/product) | Free: 100 requests a month, non-commercial, live flights only; Basic $49.99 a month | 1 request | times, delay, gate, terminal, status (not verified) | `access_key` query parameter |
| [AirLabs](https://airlabs.co/) | Free package with unstated limits; about $19 a month for 10,000 | 1 query | schedules with delay and status; gate and terminal not verified | `api_key` query parameter |
| [Flightradar24 API](https://www.flightradar24.com/blog/flightradar24-api/) | Credit plans from about $9 a month (not verified) | not published | flight summaries from ADS-B and schedules | not verified |
| [Cirium (FlightStats)](https://developer.flightstats.com/getting-started/pricing), OAG | Evaluation, then quote-based enterprise contracts | not published | full status, gates, delays, alerts | not verified |
| [OpenSky Network](https://openskynetwork.github.io/opensky-api/rest.html) | Free credits a day with an account | 4 credits | ADS-B positions only; flights by aircraft from the previous day on; no schedules, gates, or delays | OAuth client credentials |
| Airport and airline developer portals | Free | 1 call | one airport's or one airline's flights only | per portal |

OpenSky has no schedules and cannot answer "is it delayed". Airport and airline APIs cover one
airport or one airline each, while a household flies several. The enterprise feeds have no price
for a household. That leaves AeroDataBox and AeroAPI; aviationstack's free tier is too small and
its paid tier too dear. AeroAPI buys about five times the lookups for free but needs a billing
card and bills past $5; the operator chose to start with no card and nothing billable.

What AeroDataBox's own documentation says, read on 2026-11-14 from its
[OpenAPI specification](https://doc.aerodatabox.com/), its [pricing](https://aerodatabox.com/pricing/),
and its [terms](https://aerodatabox.com/terms/):

- **Endpoint.** `GET /flights/{searchBy}/{searchParam}/{dateLocal}`, "Flight Status (single
  Day)", tier 2, with `searchBy` `number` and the flight number in any spacing or case, IATA or
  ICAO. `dateLocalRole` is `Departure`, `Arrival`, or `Both` (the default): whether the date is
  the local day of departure, of arrival, or either. `withAircraftImage`, `withLocation`, and
  `withFlightPlan` default to false; a flight plan bills the request twice, so it is never asked
  for. How far back and ahead a date may be depends on coverage and the plan.
- **Answer.** An array of flights, each with `number`, `status`, `codeshareStatus` (`IsOperator`,
  `IsCodeshared`, or `Unknown`), `airline`, `lastUpdatedUtc`, and a `departure` and an `arrival`
  that each hold the airport (IATA and ICAO codes, name, municipality, `timeZone`),
  `scheduledTime`, `revisedTime`, `predictedTime`, and `runwayTime` (each in UTC and local),
  `terminal`, `gate`, `checkInDesk` or `baggageBelt`, and `quality`, a list of `Basic`, `Live`,
  and `Approximate`. There is no delay field: a delay is the revised or predicted time against
  the scheduled one. `204` means no such flight; `400`, `401`, `451`, `500`, and `503` are
  documented; the marketplace adds its own refusals for rate and quota.
- **Coverage.** Live times and statuses exist only for airports AeroDataBox tracks live or by
  ADS-B; elsewhere a flight has its schedule only, which the `quality` markers say.
- **Access.** On RapidAPI the base is `https://aerodatabox.p.rapidapi.com`, and the key goes in
  RapidAPI's `X-RapidAPI-Key` header with `X-RapidAPI-Host: aerodatabox.p.rapidapi.com`. The
  RapidAPI specification names the server but not the headers, so the spike confirms them.
  API.Market's Basic plan is a seven-day trial, so the free plan is RapidAPI's.
- **Terms of the free plan.** No commercial use (art. 5.3). Attribution "clearly visible" that
  "unambiguously" identifies AeroDataBox, with a link to aerodatabox.com where technically
  feasible (art. 5.4). Retrieved data kept no longer than needed and at most seven days unless
  the response's `Cache-Control` allows longer (art. 5.5), with no permanent copies (art. 5.2(g));
  identifiers such as flight numbers and airport codes may be kept (art. 5.8). No copying or
  distributing to third parties (art. 5.2(i)); end users may use it. No circumventing quotas, and
  no second account for more quota (art. 5.2(h), 6.1.6). No use for flight operations or
  navigation (art. 5.2(a)).

## Goal

Where the operator has configured a flight status key and the user has Flight status on:

- the user can ask, in plain language or in a scheduled prompt, about any flight by its number
  and day, and the answer comes from the provider: times, delay, gate, terminal, and status, with
  AeroDataBox named as its source;
- a tracked trip's flight is followed on its day of travel, and the trip's tab gets one short
  message when the flight is delayed, moves gate or terminal, is cancelled, or is diverted, and
  nothing when nothing changed;
- a typical month, about six flight legs across the household and some questions, fits inside
  the free plan's 400 units with room to spare, and when the allowance is spent tracking stops
  and each affected user is told once.

Success: a delayed test flight on its day gives one message naming the new and old departure
time, a gate change gives one line, an unchanged flight gives none, and the month's units stay
under the budget.

## Decisions

- **AeroDataBox's free plan on RapidAPI is the provider, with no fallback.** It needs no card and
  cannot bill: past its 400 units the marketplace refuses. A private household assistant that
  nobody pays for is the non-commercial use the plan allows. A second provider at run time is
  not built: two keys, two privacy lines, and two shapes of data that disagree cost more than an
  hour of "could not check" on a rare outage. The provider sits behind an interface. The upgrade
  path is, first, AeroDataBox's Pro plan, the same code with a larger budget in the
  configuration, and then AeroAPI's Personal tier as a second implementation of the interface.
- **One operator key, a per-user switch.** A flight number and a date are public data and are
  not tied to a user's identity at the provider, as with the NS key in plan 0027. The household
  shares one key, and the terms forbid a second account for more quota anyway. Each user has a
  `Flight status` switch in `/settings`, on by default, stored as an opt-out integration row. The
  `/privacy` line says that flight numbers and dates the user asks about or that their tracked
  trips hold are sent to AeroDataBox through RapidAPI. A booking reference, a name, or a seat is
  never sent.
- **A typed connector, `internal/connectors/flights`.** It follows the NS connector:
  - a `Provider` interface with one call, `Flights(ctx, designator, day, role)`, where the day is
    a local date and the role says whether it is the day of departure or of either departure or
    arrival, returning the normalised legs; an AeroAPI implementation would turn the day and role
    into its time window;
  - the AeroDataBox implementation: `GET /flights/number/{designator}/{day}?dateLocalRole=…`
    with images, location, and flight plan off, on a fixed base, the key in the headers and
    never in a URL or a log line, redirects refused, no proxy, a 15-second timeout, a 1 MiB body
    limit, and `httpretry` for transient failures;
  - one rate limiter in the client, shared by the tool and the tracking, that spaces requests at
    least two seconds apart, under the plan's one a second and 1,000 an hour. It is a mutex and a
    time, with no new dependency;
  - a normalised `Leg`: designator, codeshare status, operating airline, origin and destination
    (IATA code, name, time zone), scheduled, revised or predicted, and runway departure and
    arrival, the delay at each end computed from them, gate and terminal at each end, whether the
    times are live, and the status;
  - error classes as NS has them, each one sentence safe for the model and the log: unauthorized,
    not found (`204`), rate limited, allowance spent, unavailable (`451`, `5xx`), timeout,
    unexpected response.
- **A designator is checked and normalised before it is sent.** Uppercased with spaces removed,
  it is an airline code of two letters or digits, not both digits (`EX`, `U2`, `3U`), or of three
  letters, then one to four digits with an optional letter. Leading zeros of the number are
  removed, so `EX 0123` and `EX123` are one designator and one cache entry. Anything else is
  refused without a lookup.
- **A codeshare is the flight it is sold as, when the provider finds it.** AeroDataBox searches
  the number a flight "is being operated under". The spike records what a marketing number
  returns. When it returns a leg marked `IsCodeshared`, that leg is taken and messages name it as
  asked, with the operating airline once: `EX123 (operated by Other Air)`. When it returns
  nothing, the tool and the tracking say the flight was not found under that number and ask for
  the operating airline's number, which a confirmation mail usually shows as "operated by".
- **The budget counts units, kept in SQLite and reserved before sending.** A transport under
  `httpretry` reserves each attempt's two units, retries included, before it goes out, with one
  atomic upsert that increments the period's counters only while they are under their limits and
  returns whether it did. The counters are household-wide, per quota period: one for the tool,
  one for tracking, and one per user for the tool; the purpose and the user travel in the
  request's context. Concurrent turns and the tracking therefore cannot overrun the budget, and
  it survives restarts.
  - The period starts on `HOME_AGENT_FLIGHTS_QUOTA_RESET_DAY`, the day of the month the
    operator's RapidAPI subscription renews, 1 by default, at 00:00 UTC. When a response carries
    RapidAPI's remaining-quota header, a remaining count below one lookup stops lookups as a
    spent budget does, whatever the code's own count says.
  - `HOME_AGENT_FLIGHTS_MONTHLY_UNITS` defaults to 340 of the plan's 400, 170 lookups. The 60
    units left are for the spike, the operator's own checks, and a count lost with the database.
  - Questions through the tool may use at most 100 units, 50 lookups, a month, so a chatty month
    cannot starve the tracking of a flight; tracking may use all of the budget.
  - One user's questions may use at most 50 units, 25 lookups, so one user cannot spend the
    household's.
  - An attempt that cannot be reserved is not sent and returns the allowance-spent error.
  - Results are cached for five minutes by designator, day, and role, shared by the tool, every
    user's tracking, and every trip, so two household members on the same flight cost one
    lookup.
- **A booking holds its flight number.** `trip_bookings` gains an optional `flight_number`, only
  for kind `flight`, validated as above. The scan's reply and `trip_add`/`trip_update` gain the
  field, the scan's prompt asks for it from the confirmation mail, and `trip_list` shows it. A
  connecting journey is one booking per leg, as the scan already writes them. A booking without
  a number is not tracked. The field is in the export.
- **The on-demand tool is `flight_status(flight_number, date?)`.** It is read-only, so scheduled
  turns may call it too.
  - The date is the departure day local to the origin, sent with `dateLocalRole=Departure`; it
    defaults to today in `HOME_AGENT_TIMEZONE`.
  - A date more than seven days back or ahead is answered without a lookup, to keep the units for
    flights that are near.
  - Each leg is one block: a status phrase first (`on time`, `delayed +N min`, `cancelled`,
    `may be cancelled`, `diverted`, `departed`, `landed`, or `schedule only, no live data`), then
    route, departure and arrival as scheduled and as now expected or actual, in each airport's
    local time with the airport code, gate and terminal when known, and a last line,
    `Flight data: AeroDataBox (aerodatabox.com)`.
  - The guidance says: use this tool, never web search, for a flight's status; copy the status
    phrase word for word; every time and gate must come from a result in this turn; a failed call
    is reported as a failed check, never as "on time"; and end the answer with the result's last
    line. The result is outside content, as NS results are.
- **Attribution is added by code where the user reads flight data.** The terms ask for visible
  attribution with a link where feasible, and a model can drop a line. So:
  - a chat or scheduled reply from a turn in which `flight_status` returned data gets
    `Flight data: AeroDataBox (aerodatabox.com)` appended when the reply does not already name
    aerodatabox.com;
  - every tracking message ends with the same line, appended after the 0146 rewrite, so the
    rewrite neither drops nor translates it, and the rule that strips addresses from model text
    does not touch it. Telegram shows the domain as a link.
- **Tracking runs in the trips loop, without a model turn.** A new `Flights` part of the trip
  package, beside `Updates`, is driven by the loop's cycle. The trip package declares the small
  interface it needs; the wiring adapts the connector to it, as it does the calendar reader.
  - A flight booking is tracked while its trip is planned, its owner's Trips scan job is
    enabled (the gate plan 0139 set), and the owner has Flight status on, from 38 hours before the
    booking's stored departure until the matched leg ends. The 38 hours are the 24 of the cadence
    plus the 14 by which a stored departure can be off, as below.
  - **A booking's departure is not trusted as an instant.** `trip_add` and `trip_update` read
    times in the trip's zone, the destination's, so an outbound flight typed as its local 09:30
    is stored hours off; the scan writes instants with their offset and is usually right. The
    first lookup asks for the booking's date in the trip's zone with `dateLocalRole=Both`, which
    finds a flight departing or arriving on that day; when no leg matches, one more lookup asks
    for the day before or after, whichever the stored instant points to. The leg taken is the one
    whose scheduled departure either is the stored instant or reads, at its own origin, as the
    stored wall-clock time in the trip's zone, each within 30 minutes. When none matches and
    exactly one leg was returned, that one is taken. Otherwise the line "EX123 on Mon 9 Nov was
    not found; check the flight number and time on the trip" is sent once and the booking is
    looked up again only at D−3h by the stored departure.
  - Once matched, the leg's own scheduled departure is D for the cadence, and its origin-local
    date, with `dateLocalRole=Departure`, is the day of every later lookup and of the cache key,
    the same key the tool uses. The booking itself is not changed.
  - At most one lookup runs at a time, in its own goroutine, under the loop's context and joined
    at shutdown, so start and end notices are never held up.
  - Messages are sent after the lookup is done and outside the lookup goroutine. The 0146 rewrite
    is a model call on the trip owner's own subscription and can take seconds or fail; it never
    holds up the next lookup.
- **The cadence is eight lookups a leg, on slots shared by everyone following it.** Each slot is
  a fixed time counted from D, so every trip and user following one leg is due at the same slot
  and the second is served from the cache. With D the scheduled departure and A the expected
  arrival, a leg is looked up at:
  - D−24h, the match and the baseline;
  - D−6h, skipped when it falls between 23:00 and 07:00 at the origin;
  - D−3h, D−90 min, D−45 min, and D−15 min;
  - D+30 min, which sees whether it left;
  - A+20 min, which sees whether it landed or was diverted.

  Nothing is looked up after a cancellation or after A+20 min. A leg whose times are not live,
  by the `quality` markers, is looked up only at D−24h and D−3h and gets one line, "EX123: no
  live status for this flight; check the airline's app.", since more lookups would only repeat
  its schedule. The match needs a second lookup at most once, so a leg costs eight or nine
  lookups, 16 or 18 units.

  A typical month of six legs costs at most 108 units, and with the tool's full 100 units 208,
  about half of the plan's 400; the default budget follows up to 13 legs while the tool uses its
  whole share, and 18 when it does not. When the budget left this period would not cover the
  slots of the legs still ahead, slots are dropped in this order: D−6h, D+30 min, D−90 min; the
  match, D−3h, D−45 min, D−15 min, and A+20 min are kept longest.
- **What is a change.** Each lookup is compared with the last one stored for the leg:
  - departure or arrival delay, the revised or else predicted time against the scheduled one:
    reported when the expected time moved by 15 minutes or more from the time last reported and
    is now 15 or more minutes from the schedule, or when a delay reported before has shrunk below
    15 minutes ("back on time");
  - gate or terminal: a first gate is reported only within four hours of departure, any change
    of a known gate or terminal always;
  - `Canceled` and `Diverted`: always, at once, and a cancelled leg is no longer tracked;
    `CanceledUncertain` is reported once as "may be cancelled" and the leg stays tracked;
  - not departed at D+30 min with no revised time is reported as a delay of at least 30 minutes;
  - the first lookup is the baseline and reports only a cancellation, a diversion, or a delay of
    15 minutes or more; departing and landing on time report nothing;
  - a lookup that failed, or a leg the provider no longer returns, changes nothing.
- **One message per lookup that found changes, written by code.** It goes to the trip's tab with
  the existing `Sender`, through the rewrite of plan 0146 into the tab's language, and is handed
  to the user's next turn as outside content, as trip updates are. Examples, for a made-up flight:
  - `EX123 Example City → Other City: departure delayed 45 min, now 14:25 (was 13:40).`
  - `EX123: gate B12 (was B7), terminal 2.`
  - `EX123 is cancelled. Check the airline's app for rebooking.`
  - `EX123 is diverted to Third City (TCY).` (or without the airport when the provider does not
    name it)
  - `EX123 is back on time: departs 13:40.`

  Times are local to the airport they belong to. The message ends with the attribution line
  above. Whether something changed is decided by comparing with the stored last lookup, not by
  the sent log. Each change is then recorded as a `SentUpdate` with source `flight` and a
  reference of designator, date, field, new value, and the lookup's time, so a gate that goes
  B7, B12, B7, B12 or a delay that comes back is reported each time, and the 0139 check's prompt
  lists what was sent. `trip_updates_sent` allows only `mail`, `web`, and `calendar` as a source,
  so its table is rebuilt to allow `flight`.
- **Failures are said once, not repeated.**
  - A transient failure is retried by `httpretry`; a lookup that still fails records nothing and
    the leg waits for its next slot.
  - When two slots in a row inside D−3h fail for a leg, the tab gets one line, "Flight status
    for EX123 could not be checked; check the airline's app.", once per leg.
  - **When the allowance is spent,** by the code's budget or by the marketplace refusing for
    quota, tracking stops for every leg until the next period starts, without errors in a loop:
    no lookup is attempted, the legs keep their last stored state, and a leg whose slots all
    passed meanwhile simply ends. Each user with a flight in the tracking window gets one line in
    that trip's tab, once per period: "Flight tracking is paused until Tue 1 Dec: the household's
    free flight-status allowance for this month is used up. Check the airline's app." The tool
    answers with the same sentence and no lookup. The operator gets a warning log line and the
    metric below. When the period starts, tracking picks up at the next slot of each leg.
  - When only the tool's share is spent, the tool says so and tracking goes on.
  - A refused key is logged as `unauthorized` and every lookup is skipped until restart, since
    each retry would fail alike; the tool says the operator needs to check the key.
- **State, and the seven days the terms allow.** Migrations are numbered 058 to 060, fixed across
  the plans drafted beside this one, in chunk order: 058 the unit counters, 059 the booking's
  flight number, 060 the flight state and the rebuilt `trip_updates_sent`. The flight state keeps,
  per trip and leg (designator and origin-local date), the last stored lookup as JSON, the last
  attempt, the last success, and which once-only lines were sent. It is left out of the export,
  as the 0139 update state is. The loop's hourly prune deletes a leg's stored lookup, and the
  text of its `flight` items in the sent log, seven days after the leg ends; the designator, the
  date, and the airport codes may stay, as identifiers. The unit counters are their own table,
  also left out of the export.
- **Operations.** `HOME_AGENT_FLIGHTS_API_KEY` or `HOME_AGENT_FLIGHTS_API_KEY_FILE` turns the
  connector on, as the NS key does; production reads it from a Keychain item that
  `scripts/nas-sync-secrets` materialises as a Docker secret, and `HOME_AGENT_FLIGHTS_ENABLED`
  adds a `deploy/compose.app.flights.yaml` overlay. `HOME_AGENT_FLIGHTS_MONTHLY_UNITS` and
  `HOME_AGENT_FLIGHTS_QUOTA_RESET_DAY` are optional. Metrics: lookups by outcome and purpose, and
  the period's units against the budget.
- **Not in this plan:** a flight row on the day card or a status widget, the provider's paid
  flight alerts, departed and landed messages, seat or booking changes, and flights not on a trip
  being followed on their own.

## Approach

1. **Spike** by the operator once an AeroDataBox key exists on RapidAPI's free Basic plan:
   confirm that the plan took no card and stops at its quota, the header names, the quota
   headers, and the renewal day; `curl` the flight status endpoint for a flight on time, a
   delayed one, a cancelled one, and one by a marketing number, on flights no household member is
   on; save trimmed responses as test fixtures with neutral values; and record here how a delay,
   a diversion, a codeshare, and two legs under one number look, and how far back and ahead the
   plan answers. The spike spends about 20 of the 60 spare units. Anything that contradicts the
   decisions is amended here before building.
2. **Connector**: config, client, reserving transport and unit counters, cache, AeroDataBox
   implementation, switch, the tool and its rendering, attribution, wiring, and the settings and
   privacy lines.
3. **Flight number on the booking**: migration, repository, scan reply and prompt, trip tools,
   export.
4. **Tracking**: state table, the `Flights` part with its cadence, leg matching, comparison,
   messages, failure lines, the pause when the allowance is spent, the seven-day prune, and the
   loop's hook.
5. **Documentation and operations.**

## Implementation chunks

1. **Spike and fixtures.** Simple. The fixtures can be built first from the response examples in
   AeroDataBox's published OpenAPI specification, so chunks 2, 3, and 5 need not wait; the spike,
   with the AeroDataBox key the operator creates, replaces them. Files: this plan,
   `internal/connectors/flights/testdata/`. Accepted when the fixtures, taken from flights no
   household member is on, cover on time, delayed, cancelled, diverted, schedule-only, two legs
   under one number, and a marketing number; and this plan records the confirmed headers, quota
   behaviour, renewal day, date range, and the codeshare rule.
2. **Connector client and budget.** Complex. Depends on 1's first fixtures. Files: new
   `internal/connectors/flights/` (client, provider, AeroDataBox, legs, budget, cache, service),
   `internal/config/flights.go`, migration `058_flight_lookups.sql` and its repository in
   `internal/store/sqlite`. Accepted when a fake AeroDataBox over `httptest` shows: the key only
   in the headers; no image, location, or flight plan asked for; a malformed designator sends
   nothing, and `EX0123`, `U2123`, and `3U123` normalise as above; the cache serves a repeat;
   retried attempts are counted at two units each; concurrent requests at the limit reserve no
   more than it; the budget, the tool's 100 units, and a user's 50 refuse without a request and
   survive a restart on the same database; the period turns on the reset day; a remaining-quota
   header below one lookup stops lookups; a marketplace quota refusal becomes the allowance-spent
   error; requests are spaced by the limiter; `204` is not found; each error class is one
   sentence without the key or the body.
3. **`flight_status` tool and wiring.** Complex. Depends on 2. Files:
   `internal/connectors/flights/tools.go` and rendering, `internal/wiring/wiring.go`,
   `internal/telegram/switch_pages.go` (the switch page, its help topic, and its settings entry),
   `internal/telegram` (privacy line), `internal/prompt` if the guidance needs it, and
   `internal/turn` where a turn's final reply is put together, for the attribution line. Accepted
   when the tool is offered only with a key and the switch on, renders each status phrase, a
   schedule-only leg, and the attribution line, answers a date out of range without a lookup,
   names a codeshare as asked or asks for the operating number, says "check failed" when the fake
   provider fails, and a reply that used it and left out the attribution gets it appended.
4. **Flight number on bookings.** Simple. Independent of 2 and 3. Files:
   migration `059_trip_booking_flight_number.sql`, the trip repository, `internal/trip/trip.go`, `scan.go`,
   `service.go`, `tools.go`, `export.go`. Accepted when a scan reply and `trip_add` store a
   number, a malformed one is refused with a sentence, a non-flight booking cannot have one, a
   trip update keeps it, and an export and restore round-trip it.
5. **Tracking and change messages.** Complex. Depends on 2 and 4. Files:
   `internal/trip/flights.go` and its integration test, `internal/trip/loop.go`, migration
   `060_trip_flights.sql` (the flight state and `trip_updates_sent` rebuilt to allow `flight`)
   and its repository, `internal/wiring/trips.go`. Accepted when, on a temporary database with a
   fake AeroDataBox, a fake clock stepped from slot to slot, and the Telegram simulator: a leg
   costs the eight lookups above from D−24h to A+20 min, and a schedule-only leg two; nothing is
   looked up outside the window, with the scan job off, or with the switch off; an outbound
   booking stored in the destination's zone and one stored as a true instant both match their
   leg, with at most one extra lookup; the baseline is silent for an on-time flight; a 45-minute
   delay, a gate change, a cancellation, a possible cancellation, a diversion, and a return to on
   time each give the line above once; a gate going B7, B12, B7, B12 gives three lines; a
   10-minute wobble gives nothing; two users on the same flight, stepped to the same slot, cost
   one lookup; a rewrite that hangs delays no lookup; two failed slots before departure give one
   line; a spent allowance gives one line per user per period, attempts no lookup until the
   period turns, and resumes at the next slot; a short budget drops slots in the order above;
   every message ends with the attribution line in English after the rewrite; a leg's stored
   lookup is gone seven days after it ends; shutdown during a lookup returns promptly; and the
   message reaches the trip's tab in its language through the 0146 rewrite.
6. **Documentation and operations.** Simple. Depends on 3 and 5. Files: `README.md` (trips,
   settings, privacy, attribution), `docs/architecture.md`, `docs/operations.md` (RapidAPI
   sign-up on the free plan, key rotation, budget and reset day, the upgrade path),
   `.env.example`, `deploy/nas.env.example`, `deploy/compose.app.flights.yaml`,
   `scripts/nas-sync-secrets`, `scripts/deploy-app-nas`, and the operator's local `.env` and
   `deploy/.env.nas`. Accepted when a fresh reader can configure the key from the docs alone.

## Files affected

- new `internal/connectors/flights/` with tests and fixtures
- `internal/config/` (new `flights.go`)
- `internal/store/sqlite/migrations/` (`058_flight_lookups.sql`, `059_trip_booking_flight_number.sql`,
  `060_trip_flights.sql`) and the trip and budget repositories
- `internal/trip/` (`trip.go`, `scan.go`, `service.go`, `tools.go`, `export.go`, `loop.go`, new
  `flights.go`)
- `internal/turn` (the attribution line on a reply)
- `internal/wiring/wiring.go`, `internal/wiring/trips.go`, `internal/wiring/export.go`
- `internal/telegram/switch_pages.go` and the privacy line in `internal/telegram`
- `README.md`, `docs/architecture.md`, `docs/operations.md`, `.env.example`,
  `deploy/nas.env.example`, `deploy/compose.app.flights.yaml`, `scripts/nas-sync-secrets`,
  `scripts/deploy-app-nas`

## Validation

- Formatting, module, vet, build, race and non-race tests, and `scripts/check-plans`.
- The tests named in the chunks. The provider is faked at its HTTP boundary with the fixtures;
  the database is a real temporary SQLite file; no test reaches AeroDataBox.
- Live, by the operator after deployment: `flight_status` for a flight departing today, and a
  test trip holding a flight on its day, watching the trip's tab and the period's units in
  Grafana against RapidAPI's own usage page.

## Risks and open questions

- **The free plan is small.** 400 units are 200 lookups. Eight a leg leaves a gate change up to
  30 minutes late before departure, and a busy month drops slots. The upgrade path is
  AeroDataBox's Pro plan, $7.50–8 for 5,000 units with the same code and a larger
  `HOME_AGENT_FLIGHTS_MONTHLY_UNITS`, which also allows commercial use; then FlightAware AeroAPI's
  Personal tier, about a thousand lookups within its free $5 but with a billing card and usage
  billed past it, as a second implementation of the provider interface, with its own budget in
  money.
- **Live data depends on the airport.** Where AeroDataBox has no live feed, a flight has only its
  schedule, and a delay is never seen; the user is told once per leg.
- **Gates and terminals are sparse** outside large airports and appear late; a missing gate is
  not reported as a change.
- **A codeshare number** may find nothing, since AeroDataBox searches the operating number; the
  spike settles it, and the user is asked for the operating number when it does.
- **The terms' seven days and "no third party".** A model's answer that quoted a status stays in
  the conversation history up to 90 days, as derived text, and flight data passes through the
  user's model provider and Telegram on its way to the household member who asked. The plan
  reads that as end use; the operator decides.
- **A quota period that does not start where the code thinks** is caught by the marketplace's
  refusal and its remaining-quota header, at the cost of one refused request.
- **The cadence is a guess** at what a traveller wants. It is tuned on live trips.
- **Polling reads nothing private,** but it tells AeroDataBox and RapidAPI which flights the
  household follows, from one key.
- **Open for the operator:**
  - create the RapidAPI account and subscribe to AeroDataBox's free Basic plan for the spike,
    and confirm no card is on file; recommended;
  - read AeroDataBox's terms, articles 5.2, 5.4, 5.5, and 5.8, on retention, third parties, and
    attribution, against the reading above;
  - the budget of 340 units, the tool's 100, and a user's 50; recommended as they are;
  - the 15-minute delay threshold; recommended;
  - whether "departed" and "landed" deserve a message in a later plan; recommended not before
    the paid plan, since each costs a lookup the cadence already makes but adds noise.
