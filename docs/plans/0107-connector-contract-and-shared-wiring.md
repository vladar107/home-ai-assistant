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

- `telegram.Dependencies` replaces the positional parameters and the
  setters. Its required fields are validated as before; every other field
  may be nil. The calls left after construction are the real cycles: the
  client observing the service's tabs, the feeds handed to the scheduler,
  the watches, and the trip notifier, and the conversation's tab replies.
- `telegram.Section` is the one extension point of `/settings`. Optional
  facets, found by type assertion, add `/privacy` lines (`privacySection`),
  name an `app_help` topic (`helpSection`), or consume messages
  (`messageInterceptor`). The caller passes the sections in
  `Dependencies.Sections`, in the order `app_help` lists their topics, so
  that order stays the model-facing order it was. `settingsGroups` still
  places the pages, and `privacyOrder` places the `/privacy` lines, core
  ones (attachments, jobs, tasks, voice) and section ones in one fixed list.
  `NewService` refuses a duplicated section, one in no settings group, and
  one with `/privacy` lines but no place in `privacyOrder`.
- One switch section serves trains, buses, web access, the home, Grafana,
  and safety checks: a `SwitchPage` value per integration holds its text,
  buttons, log event, help topic, and `/privacy` line. `SwitchSettings`
  pairs a page with the principal's switch, an optional `SwitchDetails`, and
  an optional `Allowed` for the Grafana allowlist. The exported `*Line`
  constants stay. A `/privacy` line is one string, because the bus line is
  two sentences in one item.
- The home page's exposure lines are built in Telegram by `HomeDetails`,
  which takes the check as a plain function, so the composition root only
  adapts `homeassistant.Exposure` to it and Telegram no longer imports the
  Home Assistant connector. The Notion, NAS, and Google sections keep their
  connectors' status types, which they render in full.
- The bespoke sections (model, Notion, NAS, Google, heads-up, Trips,
  location, memory) get exported constructors and stay as they are. The
  model connection controller is no longer a field of `Dependencies`: only
  the model section uses it.
- The tab record and who named it move to `internal/thread` as `thread.Tab`
  and `thread.NamedBy*`, and `LoadTelegramCursor` returns the last committed
  update ID as an `int64`. The store keeps implementing `telegram.TabStore`;
  the composition root handing the store to `Dependencies.Tabs` is the
  compile-time check, so no separate assertion is added.

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
