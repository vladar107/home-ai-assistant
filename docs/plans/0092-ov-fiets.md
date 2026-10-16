# 0092 — OV-fiets availability at stations

Status: `done`

## Context

Backlog item `2be0e064` covers this. The operator added an NS API Portal
product that includes OV-fiets, but when the user asked about rental bikes,
the model searched the Home Assistant devices and found nothing. No tool
reads OV-fiets.

A live probe on 2026-10-16 with the existing operator key (`ns.api-key`)
found the following:

- `GET https://gateway.apiportal.ns.nl/places-api/v2/ovfiets?station_code=UT`
  returns `200`. The same `Ocp-Apim-Subscription-Key` works, so no new
  secret is needed.
- `payload[0].locations[]` holds one entry per rental point, with these
  fields:
  - `name`, `description`, `stationCode`
  - the address: `street`, `houseNumber`, `postalCode`, `city`
  - `open` (`Yes`/`No`), and `nextOpeningTime`, a Dutch sentence
  - `openingHours[]`, each with `dayOfWeek` (1–7), `startTime`, `endTime`,
    and `closesNextDay`
  - `extra`: `rentalBikes`, a string count; `locationCode`; and
    `serviceType`, which is `"Bemenst"` at most staffed points, missing at
    most self-service ones, and occasionally names a self-service variant
    such as `Sleutelautomaat` or `Kluizen`. `nearbyMeLocationId.value`
    starts with `Bemenst` or `Zelfservice`.
  - `open` can also be `Unknown`, with no `openingHours` at all.
  - images, links, and coordinates, which are not needed.
- One station can have several points. Utrecht Centraal and Zwolle each
  returned two.
- Without `station_code`, the whole country is about 600 KB.
  `places?type=ovfiets` returns an empty payload, and `v3` returns `404`.
- The response has `Cache-Control: max-age=60`.

## Goal

The user can ask "are there OV-fietsen at X?" or "can I get a bike when I
arrive at X?" and get the current count for each rental point at that
station, whether the point is staffed or self-service, and whether it is
open, with today's hours.

## Decisions

- **One new read tool, `ns_ovfiets`, in the NS connector.**
  - It is NS data, read with the same operator key, and it uses the same
    station names. So it sits behind the user's existing trains switch,
    with no new setting or environment variable.
  - It is offered to scheduled runs too, like the other NS reads.
- **Station-scoped.** The input is one station name or code. It is resolved
  through the existing station directory, so names and the
  several-candidates answer behave as in `ns_trips`. Each call makes one
  request, cached like the other NS results.
- **A second gateway base.** `ns.Config` gets `PlacesBaseURL`, by default
  `https://gateway.apiportal.ns.nl/places-api`. A second client shares the
  key and the retrying transport. Tests point it at the fake server. The
  app's wiring is unchanged.
- **Compact output.** Each rental point gets one line:
  - the point's name
  - staffed or self-service, when known: `serviceType: "Bemenst"` means
    staffed; otherwise the `nearbyMeLocationId.value` prefix decides
  - the bikes available now (0 reads as "none available", and a missing
    count as "count not reported")
  - open or closed now, from NS's `open`, with today's opening hours from
    `openingHours` in Dutch local time: "open all day" for `00:00`–`24:00`,
    "(next day)" when the point closes after midnight, and "closed all day"
    when today has no entry
  - the street address

  Images, links, coordinates, and the Dutch `nextOpeningTime` sentence are
  dropped. All text from NS is clipped, points of another station are
  skipped, and at most ten points are listed.
- **An empty station is a plain answer.** A station with no points gets
  "NS lists no OV-fiets rental point at X", not an error.
- **A clear message when the key lacks the product.** A `401` or `403` from
  the places API says that the operator's NS subscription does not include
  OV-fiets, so the operator knows which product to add.
- **The guidance names the tool.** A sentence in the NS guidance says:
  - OV-fiets questions go to `ns_ovfiets`, which reports the rental bikes
    available now at a station. OV-fiets is not a Home Assistant device.
  - The count is the current stock, not a forecast or a reservation. For a
    later arrival, the model says that the count can change by then.

## Approach

### Chunks

1. **Tool.** One tier below the session model, since it follows the
   existing NS tools closely.
   - `ns.Config.PlacesBaseURL` and the second client go in `service.go`
     and `client.go`.
   - The definition, decoding, and dispatch go in `tools.go`.
   - Fetching, the payload and request types, and rendering go in a new
     `ovfiets.go`.
   - Acceptance, from integration tests against a fake places endpoint
     serving a fixture trimmed from the live payload, with neutral
     stations:
     - A station name resolves to its code, and the request carries
       `station_code` and the key header.
     - The output lists each point with its count, kind, open state, today's
       hours, and address.
     - An empty payload gives the plain no-point answer.
     - `401` gives the subscription message.
     - With trains off, the tool is not offered, and a call is refused.
     - A second identical call within the cache lifetime makes no request.
2. **Docs.**
   - The README's Trains bullet.
   - The NS trains row in `docs/operations.md`: the subscription must
     include OV-fiets (places API).
   - The NS edge in the `docs/architecture.md` diagram.

## Files affected

- `internal/connectors/ns/`: `service.go`, `client.go`, `tools.go`, new
  `ovfiets.go`, `service_test.go`, `testdata/`
- `README.md`, `docs/operations.md`, `docs/architecture.md`,
  `docs/plans/README.md`

## Validation

- `gofmt`, a `go mod tidy` diff, `go vet`, `go test -race ./...`,
  `go build`, and `scripts/check-plans`.
- A live check, coordinated with the operator's GoLand server: ask how many
  OV-fietsen are at a station, and compare the count with the NS app.
- After that, close backlog item `2be0e064`.

## Risks and open questions

- `dayOfWeek` is ISO, with 1 as Monday and 7 as Sunday. The national list
  on Friday 2026-10-16 settled it: points with a later Friday closing
  reported that closing for day 5 in `nextOpeningTime`, and points listed
  for days 1–5 only were closed until Monday.
- The places API is undocumented beyond the portal, and it may change
  shape. A decode failure falls back to the usual "unexpected response"
  text.
- Follow-ups, out of scope here:
  - Finding the nearest rental point from the user's shared location, using
    the places API's coordinates.
  - An OV-fiets line in the day card or in trip reminders.
  - Watches on bike counts.
