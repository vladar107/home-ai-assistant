# 0027 — NS train information: trips, departures, and disruptions

Status: `done`

## Context

The daily scheduled prompt "Утренняя погода для офиса" already plans a day by
place and hour through `weather_forecast` (plan
[0026](0026-web-weather-and-pdf-tools.md)), but it knows nothing about the
trains between those places. The model can only guess at them through
`web_search`, which is slow, costs the user's model quota, and cannot report
live delays, cancellations, or platform changes.

NS publishes travel information through its API Portal
(`https://apiportal.ns.nl/`, gateway `https://gateway.apiportal.ns.nl/`). The
data is free but not anonymous: on 2026-09-28 a request without a key returned
`401` "Access denied due to missing subscription key", so every request needs
an `Ocp-Apim-Subscription-Key` header from a registered portal subscription
(product "Ns-App"). The operator has requested one.

Endpoints researched on 2026-09-28 from third-party clients, not yet checked
against a live key:

- `GET /reisinformatie-api/api/v3/trips` — `fromStation`/`toStation` or
  `originUicCode`/`destinationUicCode`, `viaStation`, `dateTime`,
  `searchForArrival`, `lang`. Trips carry `status` (`NORMAL`, `CANCELLED`,
  `DISRUPTION`, `ALTERNATIVE_TRANSPORT`), `transfers`, planned and actual
  duration, `crowdForecast`, messages, and legs with planned and actual times
  and tracks. Reported to return at most about five trips; paging is by a later
  `dateTime`.
- `GET /reisinformatie-api/api/v2/departures?station=<code>` — live
  departures with planned and actual time and track, cancellation, and
  messages.
- `GET /reisinformatie-api/api/v3/disruptions` (with `isActive`, `type`) and
  `/v3/disruptions/station/{code}` — disruptions, maintenance, and calamities
  with cause, advice, expected end, and affected stations.
- `GET /reisinformatie-api/api/v2/stations` — about 750 stations with codes,
  UIC codes, and short, middle, and long names; about 375 KB.

The quota is per day, but the size is unverified. Responses use offsets
without a colon (`+0200`), and `actual*` fields are missing when there is no
live data.

## Goal

Where the operator has configured an NS key, a user can ask in plain language,
or in a scheduled prompt, "Amersfoort Schothorst → Utrecht tomorrow at 08:10", "when should
I leave to be in Utrecht by 9", or "is anything wrong on my route tonight".
The answer is grounded in live NS data: the next few trips with planned and
actual times, delays, cancellations, platform changes, transfers, crowding,
and the disruptions on the route with their cause and expected end. Where no
key is configured, or the user has turned trains off, nothing changes.

## Decisions

- **One operator key, a per-user switch.** Train data is public and nothing a
  user says is tied to their identity at NS, so the household shares one key,
  stored like the Gmail OAuth client secret. Locally it lives in `.env`. In
  production, its source is a Keychain item that `scripts/nas-sync-secrets`
  materialises as a Docker secret, read through `HOME_AGENT_NS_API_KEY_FILE`.
  `HOME_AGENT_NS_ENABLED` turns on a `deploy/compose.app.ns.yaml` overlay in
  `scripts/deploy-app-nas`. A `/settings` section "Trains (NS)" is on
  by default and stored as an opt-out integration row, copying the web access
  switch from 0026, so no migration is needed.
- **A typed Go connector, not web search.** A new `internal/connectors/ns`
  provider contributes three read tools through the broker, so they get
  principal binding, deadlines, size limits, the untrusted-data prefix, audit,
  and availability in scheduled turns through `ReadOnlyToolsFor`:
  - `ns_trips(from, to, via?, date_time?, arrive_by?)` returns up to five
    trips. Each trip gives departure and arrival (planned, actual, delay in
    minutes), track and track changes, transfers with legs (train type,
    direction, and where to change), duration, crowd forecast, status, and its
    messages.
  - `ns_departures(station, date_time?, limit?)` returns the next departures
    with delay, track change, cancellation, and messages.
  - `ns_disruptions(station?, date_time?)` returns the active disruptions and
    calamities, either all of them or those touching one station, with type,
    title, cause, advice, and expected end. Maintenance is included only when
    its period covers `date_time` (default now). Results are capped at 15
    entries, with calamities first, then disruptions, then maintenance, and a
    closing line that says how many were left out, so the national list stays
    under the broker's 16 KiB result limit.
- **Stations are resolved in Go.** The station list is fetched at most once a
  day and kept in memory. A failed refresh keeps the stale list, and the first
  fetch runs under the call's own deadline. A name matches exactly on code or on any name form,
  ignoring case and accents, then by prefix and containment. An ambiguous name
  returns the candidates to the model instead of guessing. The resolved
  station codes are what the tools send to NS (`fromStation`, `toStation`,
  `viaStation`, `station`), the parameters the spike confirmed. "Utrecht" resolves to Utrecht Centraal
  and "Amsterdam" to Amsterdam Centraal, using a small preference for the
  `Centraal` station of a city.
- **Times are Europe/Amsterdam.** Input `date_time` is local time without a
  zone. It defaults to now. NS decides how far ahead it can plan (about two
  months, per the spike); its refusal is reported as a sentence. It is sent with an explicit offset, and results are printed as
  local `HH:MM`.
- **Caching protects the quota.** Trips, departures, and disruptions are
  cached for 30 seconds, keyed by the resolved request with `date_time`
  truncated to the minute, so "now" can hit. That is enough to absorb a model
  that calls twice in one turn. There is no background polling. A 429 is not
  retried; it becomes one sentence back to the model.
- **A dedicated fixed-host client, not the 0026 guard.** The guard in
  `internal/connectors/web` is unexported and exists for model-chosen URLs.
  NS has one fixed gateway host, so the connector owns a small `http.Client`:
  - a fixed base URL, redirects refused, and no proxy;
  - a 15-second timeout;
  - a 4 MiB body limit, above the station list's ~375 KB and the unfiltered
    national disruption list's ~783 KB.

  The key travels in the header, never in a URL or a log line, and error
  bodies are reduced to a status sentence.
- **`lang=en`** for API text. The model translates when it answers in Russian.
- **Guidance** tells the model:
  - use `ns_trips` for any "how/when do I get from A to B" question;
  - check `ns_disruptions` for the stations on the route when a trip reports a
    status other than NORMAL or carries messages;
  - use this, and never web search, for NS train times;
  - in a trip summary, give the earliest good option and a fallback,
    leg by leg with the weather when the weather tools are present.

## Approach

1. Spike, once the key arrives. Nothing that parses responses is built
   before it: call each endpoint with `curl` for a real
   trip (Amersfoort, Utrecht Centraal, Amsterdam), save trimmed
   responses as test fixtures, and record here the real field names, the
   quota headers, and how far ahead trips can be queried. Anything that
   contradicts the decisions above is amended here before building.
2. `internal/connectors/ns`:
   - the client and station directory;
   - the three tools and their text rendering;
   - `Status`/`SetEnabled` over the integration repository;
   - an Open-Meteo-style HTTP simulator for tests.
3. Wiring:
   - config;
   - app registration;
   - the `/settings` section and its `/privacy` line ("routes and stations you
     ask about are sent to NS").
4. Docs: `docs/02`, `docs/04`, `docs/05`, the runbook (portal signup, key
   rotation), `.env.example`, `deploy/nas.env.example`, and this plan's
   results.

## Spike results

2026-09-28, the operator's "Ns-App" key, calling
`gateway.apiportal.ns.nl/reisinformatie-api/api` with `curl`:

- **Stations.** `v2/stations` returns `200`, 375 KB, `payload[]` with
  `code`, `UICCode`, `namen.{lang,middel,kort}`, `synoniemen[]`, `land`,
  `stationType`, and coordinates. Foreign stations are included, so matching
  prefers `land == "NL"`.
- **Trips.** `v3/trips` with `fromStation=<code>&toStation=UT` returns `200`,
  about 82 KB, and exactly 5 trips, plus
  `scrollRequestForwardContext`/`scrollRequestBackwardContext` for paging.
  - Trip fields used: `status`, `transfers`, `plannedDurationInMinutes`,
    `actualDurationInMinutes`, `crowdForecast`, `punctuality`, `optimal`,
    `realtime`, `labelListItems`, `shareUrl.uri`, and the `productFare` price.
  - Leg fields used: `name` ("IC 2314"), `direction`, `cancelled`,
    `partCancelled`, `alternativeTransport`, `changePossible`,
    `crossPlatformTransfer`, `shorterStock`, `crowdForecast`,
    `transferTimeToNextLeg`, `notes`, `transferMessages`, and `product`
    (`categoryCode`, `longCategoryName`).
  - Each leg's `origin`/`destination` has `name`, `stationCode`, `uicCode`,
    `plannedDateTime`, `actualDateTime`, `plannedTrack`, and `actualTrack`.
  - Times look like `2026-09-29T08:01:00+0200`.
  - The trip carries no top-level `messages`/`primaryMessage` in a normal
    response, so disruption text comes from the leg `notes`,
    `transferMessages`, `labelListItems`, and `ns_disruptions`.
  - `searchForArrival=true` works.
- **Range.** Trips answered for yesterday through 2026-11-28. 2026-12-20
  returned `400` with `errors[].type` `INVALID_TRAVELADVICE_DATE`. The tool
  therefore has no fixed limit of its own: that error becomes the sentence
  "NS has no plan for that date".
- **Departures.** `v2/departures?station=<code>` returns `200`, 11 KB,
  `payload.departures[]` with `direction`, `name`, planned and actual time and
  track, `product`, `trainCategory`, `cancelled`, `routeStations[]`,
  `messages[]`, and `departureStatus`.
- **Disruptions.**
  - `v3/disruptions?isActive=true` returns a bare array (6 entries, 29 KB),
    and `lang=en` gives English text.
  - Entry fields: `type` (`DISRUPTION`, `MAINTENANCE`, and `CALAMITY` per
    the documentation), `title`, `start`, `end`, `impact.value`,
    `expectedDuration.description`, `publicationSections[].section.stations[]`
    (`stationCode`, `uicCode`), and
    `timespans[].{period, situation.label, cause.label, advices[]}`.
  - `v3/disruptions/station/ASD` returns `[]` while nothing touches that
    station.
  - Without `isActive`, the national list had 129 entries (783 KB): 123
    inactive planned maintenance works, 3 active maintenance works, and
    3 active disruptions. `ns_disruptions` therefore sends `isActive=true`
    only for a time within an hour of now.
- **Quota.** The responses carry no quota or rate-limit headers, so the quota
  is still unknown. The design keeps caching and keeps a 429 as one sentence.
- **Terms of use.** The portal is a JavaScript application and was not read
  from here. The operator accepted them at subscription.

## Files affected

- new `internal/connectors/ns/` (client, stations, tools, render, service,
  tests, and fixtures)
- `internal/config/` (new `ns.go`), `internal/app/app.go`
- `internal/telegram/settings.go`, `commands.go`, a new
  `internal/telegram/ns_integration_test.go`
- `.env.example`, `deploy/nas.env.example`, new `deploy/compose.app.ns.yaml`,
  `scripts/nas-sync-secrets`, `scripts/deploy-app-nas`
- `docs/02-scope-and-capabilities.md`, `docs/04-integrations.md`,
  `docs/05-security-and-privacy.md`, `docs/13-operator-runbook.md`,
  `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `go test -race ./...`,
  `scripts/check-plans`.
- Against a simulator serving the spike's fixtures:
  - an exact, an accented, a code, and an ambiguous station name;
  - a normal trip;
  - a delayed trip with a track change;
  - a cancelled leg;
  - a trip with no `actual*` fields;
  - departure-based and arrival-based queries sending the right `dateTime`
    and offset;
  - disruptions filtered by station;
  - a 401, a 429, and a 5xx each turned into one sentence without the key;
  - the 30-second cache;
  - the station list fetched once.
- Telegram integration:
  - without the key there is no section and no tools;
  - with it, trains are on by default;
  - turning them off removes the tools for that user only, and turning them
    back on restores them.
- Live, from the operator's chat:
  - tomorrow's trip asked in Russian;
  - "arrive by 9";
  - a question on a day with a real disruption;
  - the existing morning scheduled prompt, extended to include the trains.

## Risks and open questions

- The real response shapes and the quota are unverified until the spike. A
  small daily quota would call for longer caching. The scheduled prompt makes
  about 3–6 calls a day, and interactive use by two people adds some tens.
- NS terms of use for the portal must be read during the spike. Private,
  non-commercial household use is expected to be fine.
- Only NS data is covered. Arriva and other regional operators appear inside
  NS trips, but bus, tram, and metro are out of scope.
- The API may retire v2 endpoints. Each endpoint sits behind one client
  method.
