# 0043 — Shared location in conversation and location-aware tools

Status: `in-progress`

## Context

A user standing somewhere cannot ask "when is the next bus from here", "will it
rain here in the next hour", or "is there a pharmacy open near me": the model
does not know where they are, and every place-aware tool takes a name. Telegram
can share a position, either once (a pin, or a venue with a title and address)
or live for 15 minutes, 1 hour, 8 hours, or until the user stops it. Today the
bot drops both silently: `Message` has no `location` or `venue` field, so a pin
carries no text and no media and `processUpdate` ignores it as
`unhandled_text`; and `getUpdates` asks only for `message` and `callback_query`,
so the `edited_message` updates that carry live positions never arrive.

The backends already accept coordinates. Open-Meteo forecasts a latitude and
longitude, and the weather tool reaches them only after geocoding a town name.
Transitous plans trips between `lat,lon` places and reverse-geocodes a point,
and the bus stop index holds each stop group's coordinates. Nothing yet finds
shops, pharmacies, or cafés.

## Goal

A user shares a pin, a venue, or a live location in their private chat and gets
a one-line acknowledgement naming where it is. Until it expires, every turn of
theirs knows that position, so ordinary questions about "here" and "near me"
are answered for it: buses and trains from here, the departure board of the
nearest stop, the weather at this point, and nearby places of a given kind with
their opening hours. A live location keeps that position current without
producing any message. The user can see and forget what is stored from
`/settings`.

## Decisions

- **A pin is context, not a turn.** Telegram cannot attach a caption to a
  location, so the question always comes in a later message. A pin, venue, or
  the first message of a live share is stored and answered with a short
  acknowledgement from Go — `📍 Got it, near Stationsplein 1, Leiden.` or
  `📍 Following your live location until 18:30.` — without a model call.
- **Live updates are silent.** `allowed_updates` gains `edited_message`. An
  edit carrying a location for the principal's current live message updates the
  stored position; it never starts a turn, sends a message, or takes the turn
  lock. Every other edit, including edited text, is ignored with reason
  `edited_message`, as today. An edit older than the stored one is ignored, so
  out-of-order delivery cannot move the position backwards.
- **Only the latest position is kept.** One row per principal, overwritten on
  each update: no history, no track. A pin or venue is fresh for one hour; a
  live location until its `live_period` ends, or, for an open-ended share, until
  no update has arrived for one hour. A live share that ends keeps its last
  position for 15 minutes, for the question that often follows ("how far is
  it now?"). There is no retention beyond freshness: a position too old to
  answer "near me" has no use left and is only a privacy liability. Stale rows
  are invisible to every reader at once and deleted by a prune every 10
  minutes, so nothing outlives its expiry by more than that. SQLite's
  `secure_delete` is a connection setting, not a table one, so the store turns
  it on for all of its connections: deleted positions are then zeroed rather
  than left in free pages of the database file and its backups, at a small
  write cost to every table.
- **The model sees the exact position and a place name.** While a position is
  fresh, each turn's message — chat and scheduled, beside the current time from
  [0032](0032-current-time-in-turns.md) — starts with a line such as
  `[system] User's location: 52.16612, 4.48105 (±15 m; live until 18:30,
  updated 40 s ago), near Stationsplein 1, Leiden.` It goes in the message, not
  the system prompt, for the same caching reason as the time. A venue adds its
  title. With no fresh position the line is absent, and the model is told in its
  instructions to ask the user to share their location when a question needs
  one.
- **Tools take coordinates explicitly.** The model copies them from that line,
  and tools stay usable for any other point too (a venue from search results, an
  event location). Reading the store inside each tool would hide which point
  was used and break those other uses.
- **Reverse geocoding uses Transitous**, which the bus connector already calls,
  rather than adding Nominatim. It lives in the new `places` connector with its
  own client, so it works with buses switched off, and is labelled with
  `HOME_AGENT_LOCATION_CONTACT`. Results are cached
  for 24 hours on a grid of about 50 m, so a moving live location does not
  produce a request per update. A failed lookup leaves the place name out; the
  coordinates still go to the model.
- **Nearby places use OpenStreetMap's Overpass API**, the one new external
  service. `nearby_places` takes a fixed vocabulary of kinds mapped to OSM tags
  in Go (pharmacy, supermarket, bakery, café, restaurant, ATM, fuel, EV
  charging, parking, toilets, hospital, doctor, post office, parcel locker, bike
  repair), an optional name filter, a radius of 100–3,000 m (default 1,000),
  and a limit of up to 10. Overpass returns at most 100 elements, cut by
  element ID rather than distance, and Go sorts them by distance. It returns name, distance and compass direction,
  address, and the raw `opening_hours` string; the model judges "open now"
  against the current time it is given. Answers are cached for 10 minutes per
  kind and 100 m cell. Free-form Overpass queries are not exposed.
- **Coordinates in the bus tools.** `bus_trip` accepts `lat,lon` in `from` or
  `to` and plans it through Transitous itineraries, which include the walk to
  the stop. `bus_stop_status` accepts `lat,lon` in `stop` and shows the board
  of the nearest stop group within 1 km, naming its distance and the next two
  nearest groups so the model can offer them. NS tools keep taking railway
  stations only ([0038](0038-railway-stations-only.md)); a train from here goes
  through `bus_trip`, which includes trains.
- **Weather takes coordinates.** `weather_forecast` accepts `latitude` and `longitude`
  instead of `location`; exactly one of the two forms is required. The place is
  named by reverse geocoding, or "your location".
- **Validated at the boundary.** Latitude within ±90, longitude within ±180,
  both finite, and a horizontal accuracy of at most 1,500 m (Telegram's own
  limit). Anything else is refused with a reply and stores nothing.
- **Retention is configured, not compiled in.** The figures above are defaults
  of environment settings read at startup, so a restart changes them:

  | Setting | Default | Allowed |
  |---|---|---|
  | `HOME_AGENT_LOCATION_PIN_TTL` | `1h` | `5m`–`24h` |
  | `HOME_AGENT_LOCATION_LIVE_SILENCE` | `1h` | `5m`–`24h` |
  | `HOME_AGENT_LOCATION_LIVE_GRACE` | `15m` | `0`–`2h` |
  | `HOME_AGENT_LOCATION_PRUNE_INTERVAL` | `10m` | `1m`–`1h` |

  Values use Go duration syntax like `HOME_AGENT_CONVERSATION_TURN_TIMEOUT`; one
  outside its range stops startup with an error naming the setting, rather than
  quietly keeping a position for days. Freshness is computed from these values
  at read time, not stored per row, so a shorter setting applies to positions
  already stored at the next restart. `HOME_AGENT_LOCATION_ENABLED` turns the
  feature on and, like every other integration, defaults to false and requires
  conversation; while it is off, a shared location is answered with a "not
  enabled" reply and nothing is stored. Tool limits such as the
  `nearby_places` radius stay in the tool schemas, which the model reads.
- **Location is never logged.** Logs, traces, and metrics carry the source
  (`pin`, `venue`, `live`) and outcome, never coordinates, addresses, or venue
  names. Tool spans already omit arguments; this is verified for the new tools.
- **A `/settings` Location group** of its own (the overview's one-member
  rendering counts declared members, so it cannot share a group) shows `Shared: live until 18:30, near …`,
  `Shared 12 min ago, near …`, or `Not shared`, with a Forget button. `/privacy`
  gains a line on where the position goes. Forgetting
  during a live share also records that live message as ignored, so its later
  edits cannot store the position again, even after a later pin has expired;
  a new share is a new message and starts afresh. The record holds a message
  ID, not a position, so it is kept rather than pruned.

## Approach

1. `internal/telegram/client.go`: `Location` and `Venue` on `Message`,
   `EditedMessage` on `Update`, `edited_message` in `allowed_updates`.
   Complexity: simple.
2. New `internal/store/sqlite/migrations/013_user_locations.sql` and
   `internal/store/sqlite/locations.go`: the per-principal row with position,
   accuracy, source, venue title and address, live message ID, live end,
   received and updated times, and the ignored live message ID; upsert with the
   ordering rule, fresh read, forget, prune. Complexity: moderate.
3. New `internal/location/`: the freshness rules, the turn line, and the
   cached reverse geocoder behind a small interface. Complexity: moderate.
4. `internal/telegram/service.go` and new `internal/telegram/location.go`:
   route location and venue messages and location edits before media
   classification, validate, store, and acknowledge; the settings section in
   `settings.go`. Complexity: moderate.
5. `internal/conversation/service.go` and `scheduled.go`: the location line
   beside the time, through a `Locations` dependency in the config.
   Complexity: simple.
6. `internal/connectors/web/weather.go`, `tools.go`: coordinates on
   `weather`. `internal/connectors/bus/tools.go`, `stops.go`: coordinates in
   `bus_trip` and `bus_stop_status`, and the nearest-group search over the stop
   index. Complexity: moderate.
7. New `internal/connectors/places/`: the Overpass client, the kind vocabulary,
   rendering, caching, and the `nearby_places` tool, with a simulated Overpass
   server for tests. Complexity: moderate.
8. New `internal/config/location.go`: the `HOME_AGENT_LOCATION_*` settings
   with their defaults and ranges and the contact; `.env.example`,
   `deploy/nas.env.example`, and `deploy/compose.app.yaml` carry them. `internal/app/app.go`: wire the store, geocoder,
   places service, and prune. Complexity: simple.
9. Documentation: capabilities and privacy in `README.md`, the location flow
   and the new external service in `docs/architecture.md`, the Overpass
   setting in the operations guide, this plan and its index row.
   Complexity: simple.

## Files affected

- `internal/telegram/client.go`, `service.go`, `settings.go`, new
  `location.go`, new `location_integration_test.go`
- new `internal/store/sqlite/migrations/013_user_locations.sql`,
  `internal/store/sqlite/locations.go`, `locations_test.go`
- new `internal/location/`
- `internal/conversation/service.go`, `scheduled.go`, `types.go`
- `internal/connectors/web/weather.go`, `tools.go`
- `internal/connectors/bus/tools.go`, `stops.go`, `transitous.go`
- new `internal/connectors/places/`
- `internal/app/app.go`, new `internal/config/location.go`, `internal/config/config.go`,
  `.env.example`, `deploy/nas.env.example`, `deploy/compose.app.yaml`
- `internal/store/sqlite/sqlite.go` (`secure_delete`)
- `README.md`, `docs/architecture.md`, `docs/operations.md`,
  `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `scripts/check-plans`.
- Integration, through the fake Telegram server, simulated Transitous and
  Overpass, and an injected clock: a pin is acknowledged with its place name and
  no runtime call; the next message's turn carries the location line; after an
  hour it no longer does. A live share is acknowledged once; twenty edits move
  the stored position without any message or runtime call; an older edit does
  not move it back; edited text is ignored. Forget during a live share stops
  later edits from storing it. Another principal's turn never sees the
  position. Out-of-range coordinates are refused and store nothing.
- Tools: `weather` with coordinates skips geocoding; `bus_stop_status` with
  coordinates picks the nearest group and names the alternatives, and refuses
  past 1 km; `bus_trip` from coordinates plans through Transitous;
  `nearby_places` maps kinds to tags, sorts by distance, and serves a repeat
  from cache.
- Logs and spans from those tests contain no coordinate or address.
- Configuration: defaults apply when unset; an out-of-range or malformed
  duration fails startup naming the setting; a shorter pin TTL hides an
  already-stored pin.
- Live, in the operator's chat from the phone: a pin then "next bus from
  here", "weather here in the next hour", and "pharmacy open near me"; a
  15-minute live share while walking, then the same questions, checking that
  the position followed and that no message arrived per update.

## Risks and open questions

- Telegram's behaviour when a live share stops is not documented precisely:
  whether a final edit arrives without `live_period`. The live end time and the
  one-hour silence rule bound the position either way; the live test checks
  what actually arrives.
- In a dense centre more than 100 places of one kind can lie within a large
  radius, and Overpass's cut by element ID may then drop some near ones; a
  smaller radius avoids it.
- The public Overpass instance is rate-limited and sometimes slow. Caching and a
  short timeout keep a turn from hanging; a failure tells the model to fall back
  to web search.
- `opening_hours` is free text and often missing; the model may misjudge an
  edge case such as a holiday. The raw value is shown so the user can check.
- Exact coordinates go to the model provider on every fresh turn. The user chose
  this for answer quality; rounding is a small change if it is wanted later.
- Arrival and departure reminders ("remind me when I get home") build on the
  live updates stored here and the one-time reminders of
  [0040](0040-one-time-jobs-and-reminders.md); they are the next plan.
- A reply-keyboard "📍 Share location" button would save a tap on phones but
  stays on screen; left out until the attach menu proves awkward.
