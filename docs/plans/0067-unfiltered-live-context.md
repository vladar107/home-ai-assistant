# 0067 — Keep a device's sensors in the Home Assistant live context

Status: `done`

## Context

Home Assistant's MCP tool `GetLiveContext`, offered to the model as
`ha_get_live_context`, now accepts optional `name`, `domain`, and `area`
filters, and the connector passes its schema through unchanged. The model
narrows its calls with them. A `domain` filter returns only the device's own
entity and drops the sensor entities that belong to it, such as consumables,
errors, and the last run. A `name` filter matches only an exact entity name or
alias, so the device's own name matches nothing. Either way the model answers
that the data does not exist although the sensors are exposed. An `area`
filter keeps a device together with its sensors, and the unfiltered context is
far below the broker's result limit.

## Goal

The model always sees a device together with its sensors when it reads the
live context, and can still narrow the read to one area.

## Decisions

- For the live-context tool only, remove the `name` and `domain` properties,
  and any requirement of them, from the schema offered to the model.
- Drop `name` and `domain` from the model's arguments before calling Home
  Assistant, so a stale or invented filter cannot reach it.
- Keep `area`, which keeps a device's sensors with it.
- Leave the connector's own unfiltered snapshot call unchanged.

## Approach

1. Filter the live-context schema where MCP tools are mapped to broker
   definitions.
2. Remove the filters from the arguments in the tool execution path.
3. Cover both with an integration test against the simulated MCP server.

## Files affected

- `internal/connectors/homeassistant/snapshot.go`
- `internal/connectors/homeassistant/tools.go`
- `internal/connectors/homeassistant/service_test.go`

## Validation

- Integration test: the offered schema lacks `name` and `domain` but keeps
  `area`, and a call carrying all three forwards only `area`.
- Format, vet, test, build, and plan checks.
- A live question about a device's sensors after deployment.

## Risks and open questions

- A whole-home read is larger than a filtered one; it stays well inside the
  result limit today, and `area` remains for narrowing.
