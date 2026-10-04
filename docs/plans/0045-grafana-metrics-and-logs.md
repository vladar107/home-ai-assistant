# 0045 — Grafana metrics and logs in conversation

Status: `done`

## Context

The household's telemetry already lives in Grafana on the NAS: Prometheus
scrapes the NAS (node and SMART exporters), UniFi, Immich, Grafana, Alloy, and
Tempo, and receives the agent's own metrics over OTLP; Loki keeps 14 days of
container logs from both Compose projects and AdGuard's DNS query log; eleven
provisioned dashboards with stable UIDs (`nas-health`, `home-agent-overview`,
`immich`, the UniFi set, …) hold the queries worth asking. Reading any of it
today means opening Grafana on a laptop. The operator cannot ask the assistant
"is the NAS healthy?", "how full is the volume?", or "any errors from Immich
since this morning?".

The agent container already joins the external `home-observability` network
(`deploy/compose.app.yaml`), so Grafana is reachable at `http://grafana:3000`
without publishing anything new. Prometheus, Loki, and Tempo are not published
at all; Grafana's data source proxy is the one supported way in.

## Goal

The operator asks ordinary questions about the home's infrastructure in their
private chat and gets answers grounded in current metrics and logs: health and
capacity of the NAS, network and client state from UniFi, Immich and agent
behaviour, recent errors from any container. The same tools work in scheduled
turns, so a recurring brief ("every morning, tell me if anything on the NAS
looks off") needs no new mechanism. No other household member is offered the
tools.

## Decisions

- **Operator-only, by configuration.** `HOME_AGENT_GRAFANA_PRINCIPALS` lists the
  principal IDs that may use Grafana; everyone else never sees the tools, in
  `/settings` or in a turn. Telemetry is the operator's infrastructure, and
  logs show other household members' activity (which tools their turns used,
  which devices they reached), so this is a boundary, not a preference.
- **One read-only service account, held by the operator.** A Grafana service
  account `home-agent` with the `Viewer` role, its token stored in Keychain as
  `grafana.home-agent-token` (account `home-ai-assistant`) and deployed as a
  file read through `HOME_AGENT_GRAFANA_TOKEN_FILE`, following
  `HOME_AGENT_HA_TOKEN_FILE`. It is separate from the `claude-mcp` account so
  either can be revoked alone. Viewer cannot change dashboards, alert rules,
  silences, or data sources, so no tool here can either, whatever the model
  sends.
- **Enabled by default for allowed principals, with an opt-out** in `/settings`,
  stored as an `integration_connections` row exactly as Home Assistant does it.
  There is no per-user credential to connect.
- **Raw PromQL and LogQL, bounded by the connector.** Curated questions would
  cover only what was anticipated; the value is in follow-ups. Queries are
  read-only by construction, so the risk is cost and size, and the connector
  owns both: a range at most 7 days, at most 500 points per series and 20 series
  returned, at most 200 log lines with each line truncated, and the broker's
  16 KiB result cap. When a bound bites, the result says so and how to narrow
  the query, rather than silently dropping data.
- **Dashboards are the map.** A dashboard tool lists dashboards and, for one
  dashboard, its panels with titles, units, and their PromQL/LogQL targets, so
  the model reuses the household's curated queries instead of guessing metric
  names. Tool guidance also names the main metric families and Loki labels
  (`compose_project`, `container`, `service_name`).
- **Logs are third-party content.** Log lines carry text from the network
  (DNS names, HTTP paths, Immich file names), so `grafana_logs` is
  `ThirdParty` and `Screen`. Metrics are numbers and labels from the household's
  own exporters and are `ThirdParty` without screening.
- **Results are compact text, not raw JSON.** Range results become per-series
  summaries (labels, first/last/min/max/mean, and a downsampled sequence);
  instant results become a table; logs become `time container line`.
- **Timestamps in the user's zone.** Times in results are rendered in
  `HOME_AGENT_TIMEZONE`, matching the current-time line in every turn.
- **No panel images.** Rendering needs the Grafana Image Renderer, a separate
  headless-Chromium service the NAS does not run; it is out of scope.
- **No alerting.** Reading firing alerts, forwarding them, and managing
  silences are out of scope; Hearthwatch keeps delivering alerts.

## Approach

1. **Config.** `internal/config/grafana.go`: `HOME_AGENT_GRAFANA_URL` (default
   `http://grafana:3000`), `HOME_AGENT_GRAFANA_TOKEN` or
   `HOME_AGENT_GRAFANA_TOKEN_FILE` (at most one, same file checks as Home
   Assistant), `HOME_AGENT_GRAFANA_PRINCIPALS` (comma-separated principal IDs,
   checked for shape as the NAS roots file is). The feature is on when a token
   is set; a token without principals is a configuration error.
2. **Client.** `internal/connectors/grafana/client.go` over Grafana's HTTP API
   with a bearer token and its own request timeout: `/api/search` and
   `/api/dashboards/uid/{uid}` for dashboards; the data source proxy
   (`/api/datasources/proxy/uid/prometheus/api/v1/query{,_range}` and
   `/api/datasources/proxy/uid/loki/loki/api/v1/query_range`) for queries.
   Errors are classified (unauthorized, forbidden, not found, unavailable,
   timeout) without echoing provider text, except a rejected query, whose data
   source message the model needs to fix its query; it is clipped to 300
   characters.
3. **Tools.** `internal/connectors/grafana/tools.go`, all `RiskRead`:
   - `grafana_dashboards` — without `uid`, list dashboards (uid, title,
     folder); with `uid`, list that dashboard's variables, panels, units, and
     targets, rows flattened.
   - `grafana_metrics` — `query`, optional `range` (a duration such as `6h`),
     `end` (RFC 3339, default now), and `step`; instant when no range is given.
   - `grafana_logs` — `query`, optional `range` (default `1h`), `end`, `limit`
     (default 50), and `direction`. A metric query such as `count_over_time`
     is summarised as series.
   A step finer than range/500 is widened and the result says so; the default
   step gives about 120 points, never below 15 s. `Contribute` returns nothing
   unless the principal is allowed and has not opted out; `Execute` re-checks
   both.
4. **Settings.** A "Monitoring" group in `/settings` with an on/off switch,
   present only for allowed principals: a settings section may now be
   restricted to some principals, and is then absent for everyone else, so a
   forged button resolves to nothing. `/privacy` names it while it is on.
5. **Wiring.** Construct the service in `internal/app/app.go` when configured,
   register it as a module and with the tool broker, and hand it to the
   Telegram service with `SetGrafana`.
6. **Deploy and docs.** A `deploy/compose.app.grafana.yaml` overlay, applied
   by `scripts/deploy-app-nas` while `HOME_AGENT_GRAFANA_ENABLED` is true, sets
   the token file, URL, and principals; `scripts/nas-sync-secrets`
   materializes the token as `grafana-token` beside `ha-token`. The new
   settings go to `.env.example`, `.env`, and `deploy/.env.nas` (disabled
   until the token is in Keychain). `docs/operations.md` documents creating
   the service account and storing its token; `docs/architecture.md` gains the
   connector and its scope.

## Files affected

- `internal/config/grafana.go`, `internal/config/config.go`
- `internal/connectors/grafana/` (client, service, tools, rendering, tests)
- `internal/telegram/grafana.go`, `internal/telegram/settings.go`,
  `internal/telegram/service.go`
- `internal/app/app.go`
- `internal/telegram/grafana_integration_test.go` and the shared harness
- `deploy/compose.app.grafana.yaml`, `scripts/deploy-app-nas`,
  `scripts/nas-sync-secrets`, `deploy/.env.nas`, `.env.example`
- `docs/operations.md`, `docs/architecture.md`

## Validation

- Integration tests against an `httptest` Grafana stand-in: allowed vs. other
  principals see or do not see the tools and the settings entry; opt-out takes
  effect mid-turn; range, series, line, and size bounds are applied and
  reported; error classes map to model-facing text; the bearer token never
  appears in results or logs.
- `go test ./...`, vet, build, and `scripts/check-plans`.
- Live: on the NAS, ask about NAS health, volume usage, UniFi clients, and
  recent Immich errors; confirm a scheduled read-only turn can query metrics;
  confirm a non-operator principal is offered nothing.

## Risks and open questions

- The model may write expensive queries (wide regexes over Loki, high
  cardinality PromQL). Bounds cap what is returned, not what Grafana computes;
  the client timeout and Loki's own limits are the backstop. Watch Grafana and
  Loki load after rollout.
- Loki holds other household services' logs and the agent's own, which carry
  principal IDs, tool names, and outcomes but never message text (the quiet
  telemetry rule in `docs/architecture.md`). Operator-only access is the
  mitigation; no project is excluded from `grafana_logs`.
- Confirm the `Viewer` role is allowed to query through the data source proxy
  on Grafana 13 with the provisioned data sources; if not, grant the
  `datasources:query` permission explicitly rather than raising the role.
- Tempo is left out until there is a concrete question it answers better than
  metrics and logs.
