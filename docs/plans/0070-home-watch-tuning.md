# 0070 — Home watch tuning after the first conversation

Status: `done`

## Context

The first conversation on the NAS after plans 0068 and 0069 went live
showed four rough edges:

- **The model didn't think of a watch.** The user asked three times when
  the robot vacuum would start cleaning. The model read the schedule twice
  and only proposed a home watch after "I'm not asking about the schedule".
  The watch guidance says what a home watch is, but not to propose one
  when someone asks to be told when something happens.
- **A missing weekday blocked the proposal.** The first `watch_propose`
  failed because it gave neither weekdays nor a date, so the model asked
  "every day or specific days?" before proposing again.
- **An unchecked state was accepted.** The watch targets a vendor status
  sensor with a state, `cleaning`, that nothing checked the sensor can
  report. The device's own `vacuum` entity has Home Assistant's standard
  states.
- **Home actions waited for Confirm.** Both schedule reads were proposed
  for confirmation rather than run. The conversation had started with
  `grafana_alerts`, which is marked as outside content, so every home
  action after it needed Confirm. Alert names and summaries come from the
  household's own alert rules.

## Goal

- "Tell me when the vacuum starts" leads straight to a home watch proposal.
- That proposal succeeds the first time.
- The proposal targets a state the entity can report.
- Asking about the NAS first does not make later home actions wait for
  Confirm.

## Decisions

- **Watch guidance:** when the user asks to be told when something at home
  starts, finishes, opens, or changes, propose a home watch rather than
  reading a schedule or the current state. Prefer the device's own entity,
  such as the `vacuum`, whose states are Home Assistant's standard ones,
  over a vendor status sensor.
- **Days:** a home watch with neither weekdays nor a date runs every day.
  The confirmation shows "every day", so the user can still narrow it there.
  Transit kinds keep requiring one or the other.
- **States are checked when the entity lists them.** When the entity's
  attributes carry `options`, as enum sensors do, the resolver refuses a
  state outside them and names the valid ones. Without `options`, a state
  is accepted as before.
- **`grafana_alerts` is household data.** Its text is the household's own
  alert rules and label values. It stays third-party for memory, like the
  Home Assistant tools, but it no longer marks the conversation externally
  exposed. `grafana_metrics`, `grafana_logs`, and `grafana_dashboards` are
  unchanged, since logs and labels can carry outside text.

## Approach

1. `internal/watch/watch.go`: default days for home watches.
2. `internal/watch/tools.go`: guidance on when to propose a home watch and
   which entity to prefer.
3. `internal/connectors/homeassistant/watch.go`: check states against
   `options`.
4. `internal/connectors/grafana/tools.go`: mark `grafana_alerts` as
   household data.
5. Tests for the default days, the `options` check, and the household flag.

## Files affected

- `internal/watch/watch.go`, `internal/watch/tools.go`, and tests
- `internal/connectors/homeassistant/watch.go` and tests
- `internal/connectors/grafana/tools.go` and tests

## Validation

- Tests cover:
  - a home watch without days stored as every day, with transit kinds still
    refusing a missing day
  - a state outside an enum sensor's `options` refused with the valid ones
    named
  - `grafana_alerts` registered as household data
- `gofmt`, `go vet`, `go test ./...`, `go build`, and `scripts/check-plans`.
- On the NAS:
  - "Tell me when the vacuum starts cleaning" proposes a watch on the
    vacuum entity in one turn.
  - After asking about the NAS, a home action runs without Confirm.

## Risks and open questions

- Treating `grafana_alerts` as household data trusts the alert label values,
  which come from scraped metrics. That is acceptable for the operator's own
  infrastructure, but a label carrying outside text, such as a domain name,
  would no longer gate home actions.
