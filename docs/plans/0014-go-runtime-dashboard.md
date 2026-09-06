# 0014 — Generic Go runtime dashboard and duration histogram buckets

Status: `done`

## Context

[0013](0013-runtime-metrics-and-dashboards.md) added Go, process, container,
and SQLite pool metrics and the application-specific "Home Agent Runtime"
dashboard. The operator also wants a default Go application dashboard in the
style of the common Grafana "Go processes" boards: memory, garbage collection,
goroutines, CPU, and I/O, usable for any Go service that reports through
Alloy.

Verified on 2026-09-06 against the NAS Prometheus: every duration histogram
the application records uses the OpenTelemetry SDK default bucket boundaries
(0, 5, 10, 25, ..., 10000), which are intended for milliseconds, while the
application records seconds. Sub-second Telegram, store, and health requests
all fall into the first bucket, so the p95 panels interpolate meaningless
values. `semconv/v1.43.0` provides typed observable instruments for process
disk I/O, network I/O, paging faults, and context switches, and a
`go.memory.gc.pause.duration` histogram without advisory buckets.

## Goal

A provisioned "Go Runtime" dashboard selects any reporting service by name
and shows memory, GC, goroutines, scheduler limits, CPU, I/O, and process
counters. "Home Agent Runtime" keeps only the application-specific resource
views and links to it. Latency percentiles on every dashboard use bucket
boundaries that match the recorded second-valued durations.

## Decisions

- Explicit bucket boundaries in seconds on every duration histogram in
  `internal/observability/telemetry.go`: request-style histograms (Telegram
  API, store, health) from 5 ms to 60 s; Telegram update processing and
  conversation turns from 50 ms to 300 s to cover the turn timeout;
  lifecycle phases from 1 ms to 60 s. Old series with the default `le`
  labels stay in Prometheus until retention removes them.
- New process metrics in `internal/observability/runtime.go`, all
  Linux-only readings skipped when unavailable: `process.disk.io`
  (`read`/`write`, from `/proc/self/io` `read_bytes`/`write_bytes`),
  `process.network.io` (`receive`/`transmit`, summed over non-loopback
  interfaces in `/proc/self/net/dev`, which is the container namespace on
  the NAS), `process.paging.faults` (`major`/`minor`) and
  `process.context_switches` (`voluntary`/`involuntary`) from `getrusage`.
- `go.memory.gc.pause.duration` is fed from the `/gc/pauses:seconds` runtime
  histogram: the callback records the count delta of each runtime bucket at
  that bucket's upper bound into a histogram with explicit boundaries from
  10 µs to 100 ms. Scheduler latency (`/sched/latencies:seconds`) is not
  exported the same way because it counts every scheduling event and would
  cost thousands of records per collection for no operational decision.
- Dashboards remain provisioned JSON. "Go Runtime" (`uid: go-runtime`) has a
  `service` variable from `label_values(go_goroutine_count, service_name)`
  and no application-specific panel. "Home Agent Runtime" drops the Go
  memory, Go CPU, GC, and goroutine/thread panels that move there and keeps
  uptime, version, container CPU, memory, and PIDs versus limits, SQLite
  pool and waits, and a link to "Go Runtime" for the selected service.

## Approach

1. Bucket boundaries and the new instruments and readers in the
   observability package. Complexity: complex.
2. `deploy/grafana/dashboards/go-runtime.json` and the trimmed
   `home-agent-runtime.json`. Complexity: complex.
3. `docs/08-observability-and-operations.md` dashboards and
   `docs/13-operator-runbook.md` §8 verification list.
4. Operator: deploy monitoring, then the application; verify both dashboards
   and that p95 panels now report sub-second values for Telegram API calls
   other than long polling.

## Files affected

- `internal/observability/telemetry.go`, `internal/observability/runtime.go`
- `deploy/grafana/dashboards/go-runtime.json`,
  `deploy/grafana/dashboards/home-agent-runtime.json`
- `docs/08-observability-and-operations.md`, `docs/13-operator-runbook.md`

## Validation

- `gofmt`, `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`,
  `go build ./...`, `scripts/check-plans`.
- A temporary manual-reader check shows the new instruments and the
  explicit `le` boundaries; it is not committed.
- Dashboard JSON parses, every expression is accepted by the NAS Prometheus
  through the Grafana proxy, the variable query resolves to `home-agent`.
- Live after deployment: "Go Runtime" shows the application's memory, GC
  pauses, goroutines, CPU, disk, and network activity; the overview p95
  panels show sub-second Telegram `sendMessage` latency.

## Risks and open questions

- Recording GC pause deltas happens inside the metric callback; the number
  of records per collection equals the number of pauses since the last
  collection, which is small for this application.
- `/proc/self/io` and `/proc/self/net/dev` are absent on macOS; the panels
  are empty for local runs.
- Changing bucket boundaries breaks percentile continuity across the deploy
  boundary for the retention window.
