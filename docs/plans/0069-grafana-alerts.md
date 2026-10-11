# 0069 — Grafana alerts in conversation

Status: `done`

## Context

The monitoring repository provisions Grafana alert rules for:

- the NAS's disks, volume, and memory
- scrape targets
- the application
- UniFi devices
- internet outages
- Home Assistant and its backups

A separate send-only bot delivers them to the operator's chat, so alerts
still arrive when the assistant is down. That delivery path stays as it is.

Plan 0045 lets the operator read metrics and logs in conversation through a
read-only `Viewer` service account. The assistant cannot see which alerts
are firing or what fired recently. To ask "why?" about an alert, the
operator has to forward it and describe it.

## Goal

The operator can ask "is anything wrong at home?", "what fired overnight?",
or "why is Volume filling firing?". The assistant then lists the current
alerts and the recent alert history, and digs in with the existing metrics
and logs tools. A scheduled morning report can include the alerts the same
way.

## Decisions

- **One read-only tool, `grafana_alerts`.** It belongs to the existing
  Grafana connector, under the same operator-only principal list, opt-out
  switch, and service-account token. It is not a new integration.
- **Current state comes from Grafana's Prometheus-compatible rules
  endpoint** (`/api/prometheus/grafana/api/v1/rules`). For each alert that
  is firing or pending, the tool reports:
  - its name, severity, and templated summary
  - when it became active
  - its labels

  Rules whose evaluation is failing (`error` or `nodata` health) are listed
  too, since a broken rule is also something wrong. Their error text has
  addresses replaced by a placeholder. An alert in Grafana's `Recovering`
  state counts as firing.
- **History comes from alert annotations** (`/api/annotations?type=alert`).
  The tool reports state changes within a range (default 24 hours, at most
  7 days), newest first, at most 50 entries. For each it gives the alert
  name, the previous and new state, the time, and the severity tag. The
  annotation's raw text, which includes query values, is left out.
  - A spike against the deployed Grafana 13.2.1 with the `Viewer` token
    found that `/api/v1/rules/history` answers `500 ruleUID is required to
    query annotations` without a rule, while the annotations endpoint
    answers `200` with the alert transitions.
- **Output follows the existing tools.** Rendering is compact text. Errors
  are reduced to the existing error classes. Results are treated the same
  way as `grafana_metrics` and `grafana_logs` results.
- **No writes.** Silencing, acknowledging, and editing rules stay in
  Grafana. A `Viewer` token cannot change them anyway.

## Approach

1. Client calls for the rules and history endpoints, with response
   decoding.
2. The `grafana_alerts` tool with arguments `view` (`active` or `history`)
   and `range`, plus its rendering and guidance in the tool description:
   check active alerts first, then use metrics and logs to explain.
3. README and `docs/operations.md` lines.

## Files affected

- `internal/connectors/grafana/tools.go`, `render.go`, and `service_test.go`
- README, `docs/operations.md`

## Validation

- Integration tests against a fake Grafana HTTP server cover:
  - firing, pending, and error rules
  - history range bounds and the entry cap
  - unauthorized and unavailable responses
  - annotation text being left out
- `gofmt`, `go vet`, `go test ./...`, `go build`, and
  `scripts/check-plans`.
- A live check with the `home-agent` service-account token against the
  household Grafana. It asks for active alerts and the last day of history
  in conversation.

## Risks and open questions

- Alert history depends on Grafana's default annotation history backend.
  The monitoring repository sets no `unified_alerting.state_history`
  option. If that changes, this tool changes with it.
- Alert labels can carry device names and addresses. They reach only the
  operator and their own model subscription, as the metrics tools already
  do.
