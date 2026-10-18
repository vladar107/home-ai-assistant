# 0107 — One connector contract and wiring shared with the tests

Status: `in-progress`

## Context

An architecture audit found that adding or changing an integration means editing
the same facts in many places, and that the Telegram tests run a composition
root of their own:

- `internal/app` builds every service in one long function. Each connector is
  registered by hand with the module registry, the tool broker, the approval
  service, and the watch service, and a typed-nil guard block then picks the
  interfaces handed to Telegram.
- `telegram.NewService` takes 17 positional parameters, and 13 setters add the
  rest after construction.
- Five `/settings` pages (trains, buses, web access, Grafana, safety checks)
  are the same on/off page written five times; the home page is the same page
  with extra lines.
- `/privacy`, the `app_help` topics, and the settings groups are separate
  hand-kept lists of the same sections.
- The Telegram test harness wires the services itself, in a different order
  and with different defaults than production: some services production always
  runs are only opt-in there.
- Store types (`sqlite.TelegramCursor`, `sqlite.Tab`) leak into the
  presentation layer.

Plan 0106 gave every integration one per-user switch, which this plan builds
on.

## Goal

A connector is registered with one call from one typed bundle. The Telegram
service is built from a `Dependencies` struct. One generic switch page serves
every on/off integration. Production and the Telegram test harness build the
services through the same `internal/wiring` package, so the tests run what
production runs. Telegram imports no store package. Everything users see and
everything sent to the model stays byte-identical.

## Decisions

- To be recorded as the work proceeds.

## Approach

1. Replace the positional parameters and setters of `telegram.NewService`
   with `telegram.Dependencies`. Sections are still built inside
   `NewService`, in the same order.
2. Add one generic switch section with a page description per integration,
   and optional section facets for `/privacy` lines and `app_help` topics.
   Callers pass the sections in `Dependencies.Sections`; the connector fields
   leave the Telegram service.
3. Move the tab type to `internal/thread` and return the Telegram cursor as an
   `int64`, so Telegram imports no store package.
4. In `internal/app`, bundle each connector into a typed `Connector` and
   register it with one function.
5. Extract the wiring into `internal/wiring`, which builds everything except
   the Telegram service and does not import `telegram`; `internal/app` calls
   it.
6. Build the Telegram test harness on `wiring.Build`, matching production
   where they differ, and update the tests' expectations through shared
   helpers.
7. Update the architecture document.

## Files affected

- `internal/telegram` (service, sections, help, privacy, the test harness and
  the tests whose expectations follow production)
- `internal/app`
- `internal/wiring` (new)
- `internal/thread`, `internal/store/sqlite`
- `docs/architecture.md`

## Validation

- `gofmt`, `go mod tidy -diff`, `go vet`, `go build`, the race test suite
  (the Telegram package repeatedly), and `scripts/check-plans`.
- The integration tests and the `internal/prompt` golden files show that
  user-visible text and model-facing text are unchanged.

## Risks and open questions

- The harness now runs services it used to leave out, so tests that asserted
  exact tool lists or settings pages change with it.
- The turn-engine split and module lifecycle changes are later plans.
