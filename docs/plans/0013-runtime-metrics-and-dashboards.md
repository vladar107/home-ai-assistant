# 0013 — Runtime metrics and Grafana dashboards

Status: `done`

## Context

The application emits only its own business metrics (`home_agent.*`) through
OTLP to Alloy. Verified on 2026-09-06 against the NAS Prometheus: every panel
query of the provisioned "Home Agent Overview" dashboard is valid and returns
series, but most panels look empty because there has been little traffic and
counters that were never incremented (polling retries) show "No data". A
manually imported "Golang Monitoring - Echo & GORM" dashboard expects
`go_memstats_*`, `go_goroutines`, `go_sql_*`, and `process_*` series from a
Prometheus client scrape, which this application does not expose, so it
cannot work.

The operator wants a working runtime dashboard (CPU, memory, goroutines,
connections, GC) and an overview dashboard that reflects the deployed
application. The `go.opentelemetry.io/otel` module already in `go.mod`
ships `semconv/v1.43.0` typed instruments for Go runtime, process, container,
and database-connection metrics, so no new dependency is required.

## Goal

The application reports Go runtime, process, container, and SQLite pool
metrics through the existing OTLP pipeline. Grafana provisions a "Home Agent
Runtime" dashboard built on those metrics and a revised "Home Agent Overview"
dashboard, both verified against live data on the NAS.

## Decisions

- Instrument in-house with standard library `runtime/metrics`,
  `syscall.Getrusage`, `/proc/self`, and cgroup v2 files, using the semantic
  convention names from `semconv/v1.43.0` (`goconv`, `processconv`,
  `containerconv`, `dbconv`) so the dashboard uses standard names. The
  contrib `instrumentation/runtime` module is not added.
- Go runtime: `go.goroutine.count`, `go.processor.limit`, `go.memory.used`
  (`stack`/`other`), `go.memory.limit`, `go.memory.gc.goal`,
  `go.memory.allocated`, `go.memory.allocations`, `go.memory.gc.cycles`,
  `go.cpu.time` (`user`/`gc`/`scavenge`/`idle`), `go.config.gogc`. The
  semconv histograms (`go.schedule.duration`, `go.memory.gc.pause.duration`)
  are out of scope.
- Process: `process.cpu.time` (`user`/`system`), `process.memory.usage`
  (RSS), `process.thread.count`, `process.unix.file_descriptor.count`,
  `process.uptime`. Linux-only readings are skipped when unavailable, so local
  macOS runs keep working.
- Container (cgroup v2, read only when present): `container.cpu.time`
  (`user`/`system`), `container.memory.usage`, plus
  `home_agent.container.memory.limit`, `home_agent.container.pids.count`, and
  `home_agent.container.pids.limit` for the Compose limits. These cover the
  Node/Pi child process that Go runtime metrics cannot see.
- SQLite pool from `sql.DBStats`: `db.client.connection.count`
  (`idle`/`used`), `db.client.connection.max`, and the counters
  `home_agent.store.connection.waits` and
  `home_agent.store.connection.wait_time`. The store exposes `Stats()` and the
  application registers the pool with telemetry after opening the store.
- All readings run in one observable callback per collection; nothing is
  sampled between exports.
- Alloy also copies `service.version` to the `service_version` datapoint
  label so dashboards can show the deployed commit.
- Dashboards stay provisioned JSON in `deploy/grafana/dashboards`. Rates use
  `$__rate_interval`; counters that may not exist yet display `0` instead of
  "No data". The imported "Golang Monitoring" dashboard is removed by the
  operator in Grafana; it is not provisioned.

## Approach

1. Add `internal/observability/runtime.go` with the instruments and readers,
   register it from `newTelemetry`, add `RegisterConnectionPool` to
   `Telemetry`, add `Stats()` to `sqlite.Store`, and register the pool in
   `internal/app/app.go`. Complexity: complex.
2. Extend `deploy/alloy/config.alloy` with the `service_version` label.
   Complexity: simple.
3. Add `deploy/grafana/dashboards/home-agent-runtime.json` (uptime, version,
   goroutines, threads, file descriptors; process, Go, and container CPU;
   container memory versus limit, RSS, Go memory classes, GC goal and cycles,
   allocation rate; SQLite pool usage and waits; PIDs versus limit) and revise
   `home-agent.json` (running, version, uptime, 24h turn and error totals,
   Telegram updates by outcome and reason, Telegram API rate and p95,
   polling retries, conversation turns and p95, store operations and p95,
   health requests). Complexity: complex.
4. Update `docs/13-operator-runbook.md` §8 and
   `docs/08-observability-and-operations.md` dashboards for the new dashboard.
5. Operator: deploy monitoring, then the application, and verify both
   dashboards on the NAS.

## Files affected

- `internal/observability/runtime.go`, `internal/observability/telemetry.go`
- `internal/store/sqlite/sqlite.go`, `internal/app/app.go`
- `deploy/alloy/config.alloy`
- `deploy/grafana/dashboards/home-agent-runtime.json`,
  `deploy/grafana/dashboards/home-agent.json`
- `docs/13-operator-runbook.md`, `docs/08-observability-and-operations.md`

## Validation

- `gofmt`, `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`,
  `go build ./...`, `scripts/check-plans`.
- A temporary manual-reader check lists every expected metric name from the
  telemetry package on macOS; the check is not committed.
- Dashboard JSON parses, every panel query is accepted by the NAS Prometheus
  through the Grafana proxy, and each references the `prometheus` data
  source UID.
- Live after deployment: both dashboards render in Grafana with data for the
  running application; container memory stays below the 1 GiB limit during a
  conversation turn.

## Risks and open questions

- cgroup files are only meaningful with Docker's private cgroup namespace on
  cgroup v2 (verified on the NAS); elsewhere the container panels stay empty.
- Estimated Go CPU classes are scheduler estimates and will not equal process
  CPU time; the dashboard labels them separately.
- OTel cumulative counters start at first observation; rates need two export
  intervals (one minute) after a restart before they appear.
