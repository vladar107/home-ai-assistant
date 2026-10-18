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
  `Dependencies.Sections` in any order. `settingsGroups` still places the
  pages; `sectionOrder` fixes the order `app_help` lists their topics in,
  which the model reads, and the order the message interceptors run in; and
  `privacyOrder` places the `/privacy` lines, core ones (attachments, jobs,
  tasks, voice) and section ones in one fixed list. The connectors register
  in an order the model's tool list depends on, which differs from the
  topic order, so the composition root cannot simply pass the sections in
  registration order. `NewService` refuses a duplicated section and one
  missing from `settingsGroups`, `sectionOrder`, or, with `/privacy` lines,
  `privacyOrder`.
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
- A connector bundle names what one integration adds: its module, its tool
  providers, the executor of its confirmed actions, the watch evaluators by
  kind, and its on/off page. One `register` call adds a bundle to the module
  registry, the broker, the approval service, and the watch service (and
  zeroes the watch outcome series of the kinds it registers), and keeps its
  on/off page. Every field is typed and optional, filled inside the
  integration's configuration check, so nothing is registered through
  reflection and no typed nil is handed on. Google is one bundle with two
  tool providers (mail and calendar), so the tools field is a list; the
  transit watches (departures, saved routes, disruptions) are a bundle with
  watches only, built once trains, buses, and routes exist.
- Registration keeps today's order, because the broker lists tools to the
  model in it: web access and the places connector still register among the
  core services.
- The departure, route, and disruption watch outcomes are now zeroed only
  where those evaluators are registered, as the home and agent ones already
  were.
- A bundle's on/off page holds what the page needs beyond its switch, free of
  Telegram types: the integration's switch, the Grafana allowlist, and the
  home's exposure check. The composition root finds the Telegram page by the
  integration's name (`telegram.SwitchPageFor`) and fails to start for an
  integration without one. The safety checks switch is registered as a
  bundle with only its page when TypeSafe is configured.
- `internal/wiring` builds everything except the Telegram service and does
  not import `telegram`. `wiring.Build` takes `wiring.Inputs`: the store,
  the module registry, the logger, a narrow `wiring.Telemetry` interface
  (the union of what the services record), the model runtime with its
  session files and pruner, the chat as `wiring.Chat` (the delivery
  interfaces the Telegram client implements) with the reminder, alert,
  task, and trip senders, the principals, the data directory, the household
  zone, a `CoreConfig`, and one configuration per connector, nil when not
  configured. Build fills the connector fields the caller leaves empty that
  it knows: the household zone, the credentials directory, the display
  names, the outbox, and the trains web access reads. `Assembly` holds the
  concrete services, the module registry, the switches, and the on/off
  pages. `Assembly.Start` and `Shutdown` start and stop the conversation and
  the services it runs on, still concurrently on shutdown; the modules stay
  started and stopped by `internal/app`, which keeps the started-slice
  rollback as it was.
- `internal/app` translates the configuration into `wiring.Inputs` (the
  environment's meaning, the NAS roots, the token and secret files, and the
  Pi runtime stay there), calls `wiring.Build`, and builds the Telegram
  service on the result: the dependencies, the settings sections, and the
  cycles (feeds, tab replies, `app_help`).
- The trip scan state moves to `internal/wiring` with its test. The test
  still builds the scan by hand, because it checks the state before the
  Google connector is set up, which `Build` never produces.
- `CoreConfig` carries the seams the tests need that production leaves at
  their defaults: the clock, the scheduler's tick, the approval lifetimes,
  the connection settings, the ChatGPT endpoint, and the turn indicator's
  timings. `TurnZone` is the zone of the time line each turn's message
  starts with; production always sets it to the household zone. `Recent`
  keeps turn exchanges for the guards without TypeSafe's judge. Two fields
  exist for tests only: `ExtraProviders`, offered ahead of every other tool,
  and `WatchEvaluators`, which replace or add evaluators by kind.

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
