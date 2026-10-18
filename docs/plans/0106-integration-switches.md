# 0106 — One per-user switch for each integration

Status: `done`

## Context

An architecture audit (finding F2) found that "this user turned the
integration off" has no home of its own:

- `integration_connections` models a connected account or workspace
  (`token_kind`, `workspace_id`, `workspace_name`, `bot_name`,
  `writes_enabled`), built for Notion.
- The five connectors that run on an operator key (trains, buses, web access,
  Grafana, and the home) record a user's opt-out by saving a sentinel
  connection row (`token_kind = 'internal'`, a made-up workspace such as
  `bus-off`) and switch back on by deleting it: a row present means off. Notion
  uses the same table with the opposite meaning, a row present means
  connected.
- The TypeSafe safety checks opt-in saves a sentinel row in the same table with
  the meaning "on".
- Each connector checks its own switch again in `Contribute`, `Execute`, and,
  for the home, in `ExecuteApproved`. A connector that forgets one of them
  offers or runs a tool the user turned off.

## Goal

Each principal has one on/off switch per integration, kept in its own table.
The tool broker checks it, once, before it offers an integration's tools and
before every call. The approval service checks it before it runs a confirmed or
direct action. Connectors only say which integration they belong to.
`integration_connections` keeps only connected accounts and workspaces. Every
user's current on/off state survives the migration, and `/settings` looks and
reads the same.

## Decisions

- A new `integration_switches` table (migration 040) stores only the choices
  principals made: `(principal_id, integration, enabled, changed_at)`. A
  principal without a row has the integration's default. Turning a default-on
  integration back on stores `enabled = 1` rather than deleting the row, so the
  table records the choice itself.
- Defaults live with the integration, not in the store. Each connector
  declares an `integration.Switch` value: its integration name, the name a
  refusal uses ("Train information", "The home", ...), and `DefaultOn`. Trains,
  buses, web access, Grafana, and the home are on by default. TypeSafe safety
  checks are off by default.
- The migration converts faithfully. The old switches read only whether a row
  existed for the integration, whatever it held, so every
  `integration_connections` row for `ns`, `bus`, `web`, `grafana`, or
  `homeassistant` becomes `enabled = 0`, and every `typesafe` row becomes
  `enabled = 1`, keeping its time. Those rows then leave
  `integration_connections`. Principals without a row keep their default.
- `integration.Switches` reads and changes switches over the store and logs one
  `integration.switch.changed` event when a change takes effect. It replaces
  the per-connector `*.enabled` / `*.disabled` and `judgment.optin.*` events.
  The settings sections keep their own `*.changed` events.
- A connector's tools belong to its integration when its provider implements
  `integration.Switched` (`Switch() integration.Switch`). The broker checks the
  switch before `Contribute`, so a switched-off integration contributes nothing
  and its guidance is left out. It checks again before each call, beside the
  grant check it already makes. A call to an integration switched off during
  the turn returns the switch's refusal text ("Train information is off for this
  user; they can turn it on in /settings."), with the outcome `switched_off`.
  That text is built by `prompt.ToolIntegrationOff`, beside the broker's other
  result texts.
- Approval executors use the same interface. The approval service checks the
  switch before every `ExecuteApproved`, both for a proposal confirmed later and
  for a direct action. An action whose integration is off now fails with the
  reason `disabled` and the text "<name> is off for you; turn it on in /settings
  and ask again.", the text the home connector used before.
- Wiring fails closed. Registering a switched provider with a broker that has no
  switches is an error, and so is registering a switched executor with an
  approval service that has none.
- Connectors keep reading the switch where no tool call is involved: watches,
  route legs, trip status for the day card, and whether the web search extension
  loads. They read it through `integration.SwitchReader`, so the default and the
  stored choice are interpreted in one place. Their own `Status`, `SetEnabled`,
  and sentinel constants go away, and so do their `DisabledText` constants:
  refusals use `Switch.OffText()`, which gives the same text. Grafana keeps
  `Allowed`, because the operator's allowlist is a grant, not the user's switch.
  It reads no switch itself, so its constructor no longer takes a store.
- The settings sections for trains, buses, web access, the home, Grafana, and
  safety checks now take a `telegram.SwitchController`, which
  `integration.Toggle` (one integration's switch bound to the store)
  implements. The home and Grafana sections pair it with the connector's
  exposure check or allowlist, joined in `internal/app`. Their texts, buttons,
  and callback data stay the same. The sections themselves are not generalised
  here.
- The TypeSafe opt-in moves onto the switch store. It is the same concept, a
  per-principal on/off choice, and leaving it behind would keep a sentinel row
  in `integration_connections`. `judgment.OptInService` goes away, and
  `judgment.NewOptIn` adapts the safety checks switch to `judgment.OptIn`.
- Memory stays as it is. Its switch belongs to a core feature, not an
  integration. It also gates what reaches the system prompt, and it shares a
  settings row with "learn from 👎". It already stores an explicit choice with
  no sentinel.
- The Google product switches stay on `google_accounts`. They are per connected
  account rather than per principal, and they depend on what the account's
  grant covers.
- The NAS row stays in `integration_connections`. It records a configured
  principal's access and holds its saving grant, the same role a Notion
  connection's `writes_enabled` plays. It is not an on/off switch.

## Approach

1. Add `integration.Switch`, `SwitchRepository`, `SwitchReader`, `Switched`,
   `Switches`, and `Toggle`. Add the SQLite table, the migration, and the store
   methods.
2. Give `tools.Broker` and `approval.Service` `SetSwitches`, and gate
   contributions, calls, and executions of switched providers and executors.
3. Move ns, bus, web, Grafana, and Home Assistant onto `SwitchReader` with a
   declared `Switch`. Remove their sentinel code and their switch checks in
   `Contribute`, `Execute`, and `ExecuteApproved`.
4. Replace `judgment.OptInService` with the safety checks switch.
5. Adapt the Telegram settings sections and `/privacy` to `integration.Toggle`,
   and wire the switches in `internal/app`.
6. Tests: switching an integration off hides its tools, refuses a call already
   offered, and fails a proposal confirmed afterwards. The migration preserves
   each user's state.

## Files affected

- `internal/integration/switch.go`
- `internal/store/sqlite/migrations/040_integration_switches.sql`,
  `internal/store/sqlite/switches.go`, and a migration test
- `internal/tools/broker.go`, `internal/prompt/tool.go`,
  `internal/approval/approval.go`, `internal/approval/direct.go`
- `internal/connectors/{ns,bus,web,grafana,homeassistant}`
- `internal/judgment/optin.go`
- `internal/telegram/{ns,bus,web,grafana,homeassistant,typesafe,service}.go`
- `internal/app/app.go`
- The tests that switch these integrations

## Validation

- `gofmt`, `go mod tidy -diff`, `go vet`, `go build`, the race test suite, and
  `scripts/check-plans`.

## Risks and open questions

- Another branch may also add migration 040. Whichever lands second must
  renumber its migration.
- The remaining `/settings` sections still differ in shape. A later plan
  generalises them over the switch store and the connected accounts.
