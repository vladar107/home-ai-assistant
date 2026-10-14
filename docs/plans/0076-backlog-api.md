# 0076 — Backlog over HTTP for development sessions

Status: `done`

## Context

Plan 0073 gave each user a backlog of ideas for developing the assistant. To
pick an item up, the user runs `/backlog export` in Telegram and pastes the
text into a development session, then marks it done by hand. The app already
runs an HTTP server, but it only serves `/healthz` and `/readyz`, and on the
NAS it is published on loopback alone.

## Goal

- A development session on the operator's Mac reads the backlog directly and
  marks an item done once it is implemented, with no copy and paste.
- The API is reachable from the trusted home network at a `.home` name
  through the home-edge Caddy, like Grafana and Home Assistant.

## Decisions

- **No authentication, by the operator's choice.** Network reach is the
  boundary: the operator assumes only trusted home-network clients can
  reach Caddy. The API only reads items and marks them done, so a client that
  can reach it can at worst read the backlog or close an item, which
  `/backlog` makes visible.
- **The whole backlog.** The operator is the only developer, so the API
  serves every principal's items, each labelled with its principal. There is
  no setting: the routes exist whenever the backlog does, that is, when the
  conversation is enabled. Item IDs are unique across principals, so an ID
  alone finds an item.
- **Endpoints**, JSON, under `/api/backlog`:
  - `GET /api/backlog`: all open items, oldest first, each with ID,
    principal, title, details, status, and created and updated times
    (RFC 3339).
  - `GET /api/backlog/{id}`: one item, open or done; 404 if unknown.
  - `POST /api/backlog/{id}/done`: marks an open item done and returns it;
    404 if unknown, 409 if already done. The handler loads the item by ID first and
    then calls the existing `MarkDone` with its principal, whose contract
    stays as it is.
  - Errors are `{"error": "<code>"}`.
- **Same server, one mux.** One function builds the server's handler from the
  readiness checker, the telemetry, and the backlog service (none when the
  conversation is off). `New` keeps a health-only handler; `Run` swaps in the full
  one after the Telegram and conversation services start, before the listener
  opens, so requests never arrive before the backlog service exists.
- **Telemetry.** Backlog requests use `StartHTTP` with fixed labels: endpoints
  `backlog_list`, `backlog_get`, and `backlog_done`; results `ok`,
  `not_found`, `already_done`, and `error`. Item IDs never become labels.
  The span name changes from `home_agent.http.health` to
  `home_agent.http.request`, since it now covers more than health; no
  dashboard uses it.
- **Exposure.** In the home-edge repository, Caddy serves `assistant.home`
  with `reverse_proxy home-agent:8080` over the shared `home-observability`
  network, and AdGuard rewrites the name to Caddy. The site passes only
  `/api/*` and answers 404 for everything else. The app's loopback port on
  the NAS stays as it is.
- **Usage from a session.** The development session uses `/usr/bin/curl`,
  which trusts the Home Edge Root CA from the macOS keychain.
  `CLAUDE.local.md` records how to use it.

## Approach

1. Store and service: list open items of all principals, and load an item
   by ID alone; a new `internal/backlog/handler.go` with its own JSON
   response types.
2. App: the handler-building function, and the swap in `Run`; the span name.
3. home-edge: the Caddy site, the AdGuard rewrite, and its README; deployed
   from that repository.
4. README (the backlog paragraph), `docs/architecture.md` (the component
   diagram and its edges), and `CLAUDE.local.md`.
5. Deployment: commit and push, let CI build the image, deploy it to the NAS
   with `scripts/deploy-app-nas`, then deploy home-edge. Either order is safe;
   Caddy answers 502 until the app has the routes.

## Files affected

- `internal/store/sqlite/backlog.go`, `internal/backlog/backlog.go`, a new
  `internal/backlog/handler.go`
- `internal/app/app.go`, `internal/observability/telemetry.go`
- the ignored `CLAUDE.local.md`
- a new integration test beside the handler-building function
- `~/src/home-edge`: `caddy/Caddyfile`, `adguard/AdGuardHome.yaml`, README
- README, `docs/architecture.md`

## Validation

- An integration test of the handler-building function through
  `httptest.NewServer`, with a temporary SQLite store, a real backlog
  service, and no-op telemetry:
  - list returns every principal's open items, each with its principal, and
    no done ones;
  - get returns an item in full, and 404 for an unknown ID;
  - done closes an item, a second done returns 409, and an unknown ID 404;
  - without a backlog service, the API routes return 404 and `/healthz`
    still answers.
- Formatting, module, test, vet, build, and plan checks.
- After both deployments, from the Mac: `https://assistant.home/api/backlog`
  lists the open items, `/healthz` there returns 404, and marking an item done
  twice returns 409 the second time.

## Risks and open questions

- Anyone who can reach Caddy can read the whole backlog and mark items
  done. Items hold ideas the user dictated, which may mention household
  details. Which parts of the network can reach Caddy is a firewall
  question outside both repositories. A bearer token can be added
  later without changing the endpoints.
- Deploying home-edge recreates AdGuard too, which causes a short DNS
  interruption.
