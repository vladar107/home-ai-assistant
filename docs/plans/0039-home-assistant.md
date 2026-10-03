# 0039 — Home Assistant: everyday control through its MCP server, sensitive actions confirmed

Status: `in-progress`

## Context

Home Assistant runs as its own Compose project on the NAS and is reachable at
`https://ha.home` through the home edge (Caddy, own CA). On 2026-10-02 `/api/`
answered `401` without a token and `/auth/providers` listed only the
`homeassistant` local provider. It currently holds a few media devices,
solar panels, and a robot vacuum. More devices will follow
on no fixed schedule, and adding one must not require a change here.

The architecture lists Home Assistant as not built. Home Assistant offers:

- A built-in MCP server (`/api/mcp`, the "Model Context Protocol Server"
  integration). It exposes the Assist intents as tools (turn on and off,
  media control, volume, vacuum start and dock, `GetLiveContext`, and more)
  over the entities marked "Expose to Assist", and gains intents as Home
  Assistant gains them.
- REST: states, `POST /api/services/<domain>/<service>`, `GET /api/services`
  with field descriptions, history, and `POST /api/template`.
- WebSocket: registries (areas, devices, entities with labels), exposure
  settings, and `recorder/statistic_during_period` for energy totals.

A long-lived token acts as the Home Assistant user who created it. Home
Assistant has no per-entity permissions for ordinary users.

An inventory on 2026-10-02 with a non-admin household token found:

- Solar: current production and energy produced today, over the last seven
  days, and in total, as plain sensors, plus per-inverter power. "Today" and
  "this week" are plain states; longer periods need recorder statistics.
- Vacuum (a HACS integration): `start`, `pause`, `stop`, `return_to_base`,
  `locate`, `set_fan_speed`, and `clean_area` with named rooms, plus over a
  hundred settings entities (switches, selects, buttons, map cameras).
- Media players with remotes for the TVs.
- Also present: network devices with firmware `update` entities, `person` and
  `device_tracker` locations, phone sensors, a phone `notify` service, and a
  shopping-list `todo`.
- A non-admin user can read states, `GET /api/services`, and over WebSocket
  the area, device, and entity registries, including entity labels
  (`config/entity_registry/list_for_display`). It cannot read
  `/api/template` (`401`) or `homeassistant/expose_entity/list`
  (`unauthorized`).
- The MCP server (protocol server 1.26.0) with the Assist API offers tools
  named `<domain>__<intent>`: `homeassistant__GetLiveContext`,
  `intent__HassTurnOn` and `intent__HassTurnOff` (which also lock and
  unlock), media pause, resume, next, previous, volume, mute, and search and
  play, vacuum start, return to base, and clean area, to-do list read, add,
  complete, and remove, `llm__GetDateTime`, `intent__HassCancelAllTimers`,
  and `assist_satellite__HassBroadcast`. Tools carry no read-only
  annotations. Targets are given by name, area, floor, domain, and device
  class, never by entity ID.
- `GetLiveContext` without arguments lists every exposed entity with its
  names, domain, state, areas, and device class.

## Goal

A user can ask "how much did the panels make today", "is the vacuum done",
"pause the TV", or "start the vacuum" and the assistant acts at once.
An action on a sensitive device, such as a lock or a garage door, is proposed
and runs only after the user confirms it. A new device needs no code or
deploy: the operator exposes it to Assist for everyday control, or labels it
for confirmed control.

## Decisions

- **One household token.** Home devices are shared by the household, so one
  token serves both users, like the NS key. It belongs to a dedicated
  non-admin Home Assistant user, "Home Agent", so its actions are
  attributable in Home Assistant's logbook. Locally it lives in `.env`; in
  production it is a Keychain item materialised as a Docker secret and read
  through `HOME_AGENT_HA_TOKEN_FILE`; `HOME_AGENT_HA_TOKEN` is the local
  form. `HOME_AGENT_HA_ENABLED` turns on the overlay. `HOME_AGENT_HA_URL`
  defaults to `https://ha.home`, which the Mac trusts through the edge CA.
  The NAS cannot reach the Caddy edge on its own macvlan, so in production
  the container reaches Home Assistant's host-networked port as
  `http://homeassistant:8123` through `host-gateway`, a hop that never
  leaves the NAS. Each user has a "Home" switch in `/settings`, on by
  default, copying the NS switch, and a `/privacy` line while it is on. The
  application logs which principal triggered each action.
- **Two tiers, decided in Home Assistant, not in code.**
  - *Everyday*: entities exposed to Assist. The model reaches them through
    Home Assistant's MCP tools, and actions run without confirmation.
  - *Sensitive*: entities carrying the label `assistant-confirm`. They must
    not be exposed to Assist. The model lists them with
    `ha_confirmed_devices` and reaches them only through `ha_service_call`,
    which always goes through approval. The default sensitive domains below
    only guard exposure; an entity becomes callable through a proposal only
    by carrying the label.
- **Safe default for dangerous domains.** Locks, alarm panels, covers with
  device class garage, gate or door, valves, sirens, cameras, and firmware
  `update` entities count as sensitive even without the label. A non-admin
  user cannot read the exposure settings, so the guard reads the exposed set
  from `GetLiveContext` and the labels from the entity registry, matching
  labelled entities by name. While any sensitive entity is also exposed to
  Assist, the MCP action tools are withheld, only the read tools remain, and
  `/settings` names the conflicting entities. A failed check withholds
  actions the same way and says so in `/settings`. A misconfiguration therefore
  removes everyday control instead of skipping approval.
- **MCP tools pass through the broker.** Pi never talks to Home Assistant.
  A small Go MCP client (Streamable HTTP, JSON-RPC, no new dependency) lists
  Home Assistant's tools, caches the list for five minutes, and contributes
  them as broker definitions with an `ha_` prefix. They get the same grants,
  deadlines, size limits, audit, and untrusted-data marking as every other
  tool. A tool is a read when its intent part starts with `Get` or `get_`;
  every other tool, including any Home Assistant adds later, is an action,
  and scheduled turns get reads only. `llm__GetDateTime` is dropped because
  every turn already carries the local time.
- **Generic confirmed calls.** `ha_service_call(entity_id, service, data?)`
  is a write tool. It accepts only sensitive entities, checks the service and
  its fields against `GET /api/services` for the entity's domain, and proposes
  a summary such as "Unlock Front door". Execution re-checks the entity's
  tier. No per-domain code.
- **Energy.** The solar today and seven-day sensors answer the common
  questions as plain state. `ha_history(name, period)` takes an exposed
  sensor's live-context name, so exposure stays the limit of what can be
  read, and asks `recorder/statistic_during_period` for a calendar period
  (today through last year) or a rolling 7 or 30 days, in Home Assistant's
  time zone.
- **Untrusted data.** Entity names and media titles come from devices and
  streaming apps: results are third-party and the guidance says so.

## Approach

1. Config, secret, Compose overlay, client with fixed base URL, no redirects,
   no proxy, 10-second timeout, 4 MiB body limit; the `/settings` switch.
2. MCP client and pass-through of Home Assistant's tools; read and action
   classification; guidance.
3. Sensitive-tier detection, the exposure conflict guard, and
   `ha_service_call` with proposals and approved execution.
4. `ha_history`.
5. Metrics and traces like other connectors; README, architecture (amend
   "Read first, confirm writes" for everyday home actions), and operations
   docs, including how to expose or label a new device.

## Files affected

- `internal/connectors/homeassistant/` (new)
- `internal/config`, `internal/app`, the `/settings` section in
  `internal/telegram`
- `scripts/nas-sync-secrets`, `scripts/deploy-app-nas`,
  `deploy/compose.app.homeassistant.yaml`, `deploy/nas.env.example`,
  `.env.example`
- `docs/architecture.md`, `docs/operations.md`, `README.md`

## Validation

- Integration tests against an `httptest` Home Assistant serving MCP, REST,
  and WebSocket: tool pass-through, read and action split, the conflict guard,
  sensitive detection by label and by domain, proposal and confirmed
  execution, token errors.
- Live check against `ha.home`: read the solar total and the vacuum status,
  pause and resume a TV without a prompt.

## Risks and open questions

- `person`, `device_tracker`, and phone sensors reveal where each household
  member is. With one token either user could ask about the other, so they
  stay unexposed unless both agree.
- The vacuum exposes many settings entities; exposing only the vacuum entity
  keeps the model's context small.
- Vacuum area cleaning targets Home Assistant areas, so the vacuum's rooms
  must be mapped to areas in Home Assistant for "vacuum the office" to work.
- Matching labelled entities to `GetLiveContext` by name can over-match on a
  shared name; a false match withholds actions, which is the safe side.
- MCP intents address devices by name and area, so the conflict guard, not
  per-call inspection, is what keeps sensitive devices out of everyday
  control.
- One shared token means a user's switch hides the tools, but either user can
  act on every exposed device.
- Model-misled actions on everyday devices run without a prompt; the
  exposure list is the limit of that harm.
