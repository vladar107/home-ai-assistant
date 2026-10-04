# Home Agent

A private, production-like home assistant for up to two explicitly authorized
household members. You talk to it in ordinary language through Telegram. It runs
as a Go service on the home NAS and reaches personal and home services through
narrow, per-user integrations.

Identity, authorization, secrets, observability, and recovery are treated as
product features, not afterthoughts. Every request resolves to a known user
before any model or tool is involved. Each user brings their own model
subscription and connects their own integrations, and nothing is shared or
borrowed between users.

## What it does

- **Conversation.** Natural-language chat with per-user context, backed by the
  [Pi](https://github.com/badlogic/pi-mono) agent runtime on the user's own model
  subscription. `/status`, `/privacy`, `/cancel`, `/new`, and `/settings` are
  deterministic shortcuts.
- **Notion.** Search, read pages, and query data sources. Creates and updates are
  proposed and run only after Confirm.
- **Gmail.** Read-only search and reading of the user's own mailbox.
- **NAS files.** Browse and read the user's own folders, receive files in the
  chat, and file photos into an inbox after confirmation.
- **Web.** Search, page reading, PDF text, and weather.
- **Trains.** NS trips, departures, and disruptions.
- **Home.** Reads and controls the devices exposed to Home Assistant's Assist.
  Everyday actions run immediately; sensitive devices are confirmed.
- **Buses.** Live departures, direct buses, and operator disruption notices from
  OVapi, and trips at any time or with changes planned by Transitous.
- **Shared location.** Share a pin or a live location in the chat and ask about
  "here": buses and weather at that point, and nearby pharmacies, shops, or cafés
  from OpenStreetMap. Only the latest position is kept, briefly.
- **Day cards.** A rendered image with the day's weather and trains, sent with
  the reply as its caption.
- **Schedules.** Recurring or one-time model turns ("every weekday at 07:30, plan
  my trip to work") that the user confirms and that survive restarts, and reminders
  ("remind me at five to call mom") that are set at once and can be snoozed.
- **Memory.** Each user has a small memory of instructions and facts that the
  assistant keeps across `/new`, scheduled runs, and restarts. Facts are saved
  at once and shown in the reply; instructions and anything learned after
  reading outside content are confirmed first. `/memory` lists and deletes
  entries, and `/settings` turns memory off.
- **Safety checks.** Optional, per-user TypeSafe screening of outside content,
  approvals, and tool choice.

## Quick start

Requires Go 1.27. Conversation also needs Node and the pinned Pi version.

```sh
cp .env.example .env && chmod 600 .env    # bot token, your Telegram user ID, feature flags
go run ./cmd/home-agent
curl http://127.0.0.1:8080/readyz
```

Everything beyond health endpoints is opt-in through `HOME_AGENT_*_ENABLED`
variables; `.env.example` lists them. Tests need no credentials:

```sh
go test -race ./...
```

## Documentation

| Document | Contents |
| --- | --- |
| [Architecture](docs/architecture.md) | C4 context, container, and component diagrams; security model; key decisions |
| [Operations](docs/operations.md) | Bots and users, local development, NAS deployment, integrations, rollback, backup |
| [Plans](docs/plans/README.md) | The implementation record: one plan per change, immutable once done |

Monitoring (Alloy, Prometheus, Tempo, and Grafana) lives in the separate
`home-monitoring` repository.

## Repository layout

```text
cmd/home-agent/        entry point
internal/app/          wiring
internal/telegram/     Telegram client, poller, commands, /settings
internal/conversation/ turn coordination
internal/runtime/pi/   Pi subprocess and tool bridge
internal/tools/        tool broker
internal/connectors/   notion, gmail, nas, ns, bus, web, homeassistant, places
internal/location/     shared positions and their expiry
internal/memory/       per-user memory, its tools, and prompt rendering
internal/store/sqlite/ persistence and migrations
deploy/                Compose files and configuration examples
scripts/               operator scripts (deploy, secrets, checks)
third_party/           vendored code and assets with their licenses
```
