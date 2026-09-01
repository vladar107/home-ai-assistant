# Observability and Operations

## Objectives

Observability should answer:

- Is the application running and receiving Telegram updates?
- Which integration is slow or failing?
- Did a schedule run, and was its notification delivered?
- Was a tool denied, confirmed, executed, or retried?
- Is the NAS under memory, disk, or container pressure?
- Can an operator diagnose failure without reading personal content?

## Initial stack

Start with:

- Prometheus for metrics
- Grafana for dashboards and alerting
- host/node metrics
- application `/metrics` endpoint
- structured JSON logs with local rotation
- structured audit data in SQLite

Do not deploy Loki, Alloy, or Tempo initially. Add them only when local log
search or cross-process trace correlation justifies their memory and operational
cost.

Grafana is the presentation and alert layer; it does not replace a metrics or log
data source.

## Correlation model

Use stable opaque identifiers:

```text
request_id
turn_id
principal_id (pseudonymous)
session_id
job_id
job_run_id
tool_call_id
approval_id
outbox_message_id
```

Propagate them through logs, metrics labels only where cardinality permits, and
audit records. Never use raw email addresses, paths, subjects, or prompts as
metric labels.

## Application metrics

### Ingress

- Telegram polling success/failure count
- Update processing lag
- Updates accepted/rejected by reason
- Outbound Telegram requests, latency, retries, and permanent failures

### Runtime

- Runtime process availability and restarts
- Turns started/completed/failed/aborted
- Time to first token and total turn latency
- Model/provider error class
- Input/output/cache token counts when available
- Context compaction and retry counts

### Tools and integrations

- Tool requests by capability and outcome
- Policy allow/deny/confirmation counts
- Adapter request latency and error class
- Timeouts, rate limits, and circuit-breaker state
- Result count/size buckets

### Scheduling

- Enabled jobs and overdue jobs
- Runs by status and duration
- Lease recovery count
- Missed and catch-up occurrences
- Outbox queue depth and oldest message age
- Notification attempts and delivery latency

### Process and host

- CPU, RSS, file descriptors, goroutines, and GC
- NAS memory available, swap activity, load, and filesystem capacity
- Container restart/OOM state where safely observable
- Disk/array/backup health through supported sources

## Logs

Use structured logs with levels and stable event names. Log operational events,
not conversational content.

Examples:

```text
telegram.update.accepted
principal.unknown
runtime.started
runtime.failed
tool.policy_denied
tool.completed
job.claimed
job.completed
outbox.delivery_retry
integration.circuit_open
```

Errors should be normalized into safe classes. Raw third-party responses may
contain personal content or tokens and should not be logged automatically.

## Audit

Audit differs from logs: it records security- and action-relevant facts in a
stable schema.

Minimum audit event:

```text
timestamp
actor principal
request/job origin
capability and version
resource scope reference
policy decision
approval reference, if any
attempt and idempotency reference
outcome and safe error class
duration
```

Audit retention should be longer than debug logs but still bounded. Exact
retention is an open decision based on storage and privacy preference.

## Tracing

Instrument the application with OpenTelemetry-compatible spans even before a
trace backend is installed. At minimum:

```text
telegram.receive
identity.resolve
runtime.turn
tool.policy
integration.request
scheduler.run
telegram.send
```

Initially export metrics and use correlation IDs in logs. Add Tempo only if the
cross-process Pi/tool flow becomes difficult to diagnose without trace search.

## Dashboards

### Home Agent overview

- Availability
- Telegram lag and errors
- Turn rate, latency, and failures
- Runtime restarts
- Integration health
- Pending jobs and outbox depth

### Integrations

- Per-source latency/error rate
- Rate-limit events
- OAuth refresh failures
- Circuit-breaker state

### NAS capacity

- Memory available and swap
- CPU/load
- Storage capacity and growth
- Disk/array status
- Container restarts

### Scheduler

- Upcoming runs
- Runs by status
- Missed/caught-up jobs
- Notification delivery latency

## Alerts

Infrastructure alerts go directly from Grafana to Telegram through a dedicated
alert contact path. They must not depend on the model runtime.

Initial alert candidates:

- Home agent unavailable
- Telegram polling stalled
- Runtime crash loop
- Scheduled job overdue
- Outbox oldest message exceeds threshold
- OAuth refresh repeatedly failing
- Integration circuit open for a sustained period
- NAS low available memory, swap pressure, or disk-capacity threshold
- Backup age exceeds policy

Alert thresholds should be tuned from observed baseline rather than invented as
permanent constants.

## Health endpoints

- Liveness: process event loop responds; no external dependency checks.
- Readiness: database available, migrations complete, Telegram configuration
  valid, and runtime can be started.
- Detailed admin health: per-integration status, accessible only from authorized
  local/admin paths.

An unavailable optional integration should degrade readiness only if the current
deployment declares it required.

## Retention and resource limits

Starting points:

- Metrics: 15 days
- Local operational logs: 7–14 days with size-based rotation
- Audit: 30–90 days, subject to privacy decision
- No prompt/body capture
- Scrape interval: 30–60 seconds

Measure Prometheus disk and memory growth before increasing retention or scrape
frequency.

## Backup and recovery checks

Operational readiness includes:

- automated SQLite backup status;
- age of latest successful backup;
- periodic restore exercise;
- documented token re-authorization;
- recovery of schedules without duplicate runs;
- recovery of pending outbox messages without duplicate delivery.

## Evaluation

Evals are separate from infrastructure monitoring. Maintain a small regression
set covering:

- correct tool selection;
- unknown/unauthorized user denial;
- cross-user credential isolation;
- prompt injection in source content;
- ambiguous urgent-email classification;
- integration timeout and partial briefing;
- confirmation binding and expiry;
- duplicate scheduled occurrence handling.

The set should remain small and tied to real failures. It is not a separate ML
platform.
