# 0152 — Home Assistant's MCP server: the device-control path and the full tool set

Status: `proposed`

## Context

A backlog item filed on 2026-11-12 asks that the assistant reach Home Assistant through MCP and
use the smart-home tools and entities it offers.

Part of that has existed since plan 0039, a month before the item was filed. The home connector
is built on Home Assistant's own "Model Context Protocol Server" integration. A sessionless client
posts `tools/list` and `tools/call` to `/api/mcp` with one household long-lived token from a
dedicated non-admin Home Assistant user. Every tool of the Assist API ("Device control") is
offered with an `ha_` prefix.

Reads come from a fixed allowlist (the live context, to-do items, and calendar events) plus
scripts labelled as reads. Every other tool, including tools Home Assistant adds later, is an
action. The connector also adds what Device control lacks: `ha_history` and charts from long-term
statistics, home watches, a guard that withholds actions while a device needing confirmation is
exposed, and `ha_confirmed_devices` with `ha_service_call` for labelled devices. Those two tools
always go through the approval service. Since plan 0051, an everyday action becomes a proposal
once the conversation has read outside content. Each user has a Home switch in `/settings`, on by
default.

Plan 0136 added a separate, generic path: an operator catalog of MCP servers, reads only, each
with a per-user switch that is off by default and an optional per-user key. Nothing stops the
operator from listing Home Assistant's MCP server there too.

Because the item came after plan 0039 shipped, it likely asks for more than the connector already
does. The most likely "more" is Home Assistant's new full tool set, described below. This plan
therefore covers a hardening every option needs and leaves the scope to the operator under
**Decision pending**.

### What Home Assistant's MCP server is today

Sources are the integration page (https://www.home-assistant.io/integrations/mcp_server/), the
2026.10 release notes (https://www.home-assistant.io/blog/2026/10/07/release-202610/), and
`home-assistant/core` at commit `472f85de` (`dev`, 2026-11-13). The paths below are relative to
`homeassistant/components/`, and the line numbers refer to that commit. A claim backed by neither
the docs nor the code is marked *inferred*.

- **Transport.** Stateless Streamable HTTP at `/api/mcp`: each POST stands alone, there is no
  session ID, and the answer is JSON (`mcp_server/http.py` L1–14, L68). The legacy SSE transport
  remains at `/mcp_server/sse` with `/mcp_server/messages/<session>` (`mcp_server/http.py` L17–24,
  L81–82). The docs mention neither.
- **Tool sets.** The server offers one or more "LLM APIs". Device control (`assist`) is bounded
  by the entities exposed to voice assistants. The "Home Assistant" set (`homeassistant`) is "not
  bounded by entity exposure and is restricted to admin users" (`llm/__init__.py` L140–152, L169–183).
- **Main URL options.** "Expose all AI tools" serves every set, including sets added later
  (`mcp_server/http.py` L112–119). "Require an administrator account" refuses non-admin users
  (`mcp_server/http.py` L122–124). "Read-only" keeps only tools annotated `read_only`
  (`mcp_server/server.py` L81). A new setup turns on the first two
  (`mcp_server/config_flow.py` L114–122).
- **Existing setups keep their old behaviour.** The docs do not say this; the code does. Entry
  migrations set `require_admin`, `all_llm_apis`, and `read_only` to off for an entry created
  before each option existed (`mcp_server/__init__.py` L26–51).
- **Per-set URLs ignore the options and are never read-only.** The docs say each set has its
  own URL; the code shows the rest. `POST /api/mcp/<set>` refuses non-admins for every set except
  `assist`, answers 404 for an unknown set, and always passes `read_only=False`
  (`mcp_server/http.py` L396–415). Release versions are *inferred* from merge dates in that
  file's history: per-set URLs merged on 2026-07-04 (about 2026.8), the administrator option on
  2026-08-30 (2026.10), expose-all on 2026-09-27 (2026.10), and read-only on 2026-10-12 (about
  2026.11).
- **Annotations.** Tools carry `readOnlyHint`, `destructiveHint`, `idempotentHint`, and
  `openWorldHint`, and an undeclared tool counts as a destructive write
  (`../helpers/llm.py` L191–202). The server also offers one prompt per set and, for Device
  control, a read-only live-context resource.
- **Authentication.** HTTP views validate `Authorization: Bearer <access token>`
  (`http/auth.py` L122–140). That a long-lived access token passes this check is *inferred*
  from Home Assistant's auth model; plan 0039 also observed it with the household token. The docs
  describe only OAuth 2.0 with PKCE and client ID metadata documents, without dynamic client
  registration.

Community servers exist too. The most complete (https://github.com/homeassistant-ai/ha-mcp) offers
about 86 tools, including automations, dashboards, any service, and file and configuration edits,
and expects an administrator token. It is not considered further.

### Device control compared with the full set

| | Device control (`assist`) | Full set (`homeassistant`) |
| --- | --- | --- |
| Who may connect | Any user, on `/api/mcp/assist` | Administrators only |
| Bounded by | Entities exposed to voice assistants | Nothing: every entity, device, area, label, and the system |
| Reads | Live context of exposed entities; to-do and calendar reads for exposed lists; scripts labelled as reads | Template rendering over all states and registries; search across entities, devices, areas, labels, automations, scripts, and scenes; long-term statistics of any entity by ID; system log entries and logs (and app logs where the supervisor runs); system health; config entries and integrations |
| Writes | Intent actions on exposed entities (on and off, media, volume, vacuum, timers, broadcast); exposed scripts | Any action on any target, with templates in values; reloading a config entry; starting and stepping through integration setup flows |
| Grows when | Home Assistant adds intents or the household exposes more | Integrations add tools to the set (since 2026.10, for example) |

Sources: `homeassistant/llm.py` L228–255 and L636–663; `history/llm.py` L173; `search/llm.py`
L178; `system_log/llm.py` L141, L298; `system_health/llm.py` L25; `hassio/llm.py` L58, L102;
`config/llm.py` L192, L261, L326, L601, L659.

The full set's reads answer questions Device control cannot: history of an unexposed sensor,
"why did this automation not run", or "what errors did Home Assistant log today". But every one
of them needs an administrator token. That token can do anything through REST as well, whatever
tools the connector chooses to offer. Its reads also reach entities the household deliberately
left unexposed, such as people's locations, which plan 0039 kept out unless both users agree.

## Goal

Under any option: the assistant reaches Device control through the home connector alone, pinned
to `/api/mcp/assist`. Home Assistant's main-URL options can neither break the home tools nor
widen them. The model sees each home tool once.

Under option B or C, as the operator chooses, it also reaches the chosen part of the full set
with the limits described there.

Success for the common part: against a simulated Home Assistant whose main URL requires an
administrator and serves every set, the home tools work as today with a non-admin token.

## Decisions

These hold whichever option is chosen.

- **Device control stays in the typed connector.** Not a catalog entry, and not split between
  the two paths. A catalog entry would offer reads only, by plan 0136's rule, and lose the
  exposure guard, history, watches, and the confirmed tier. Its tools would duplicate the `ha_`
  tools.
- **The connector calls `/api/mcp/assist`.** That URL serves only Device control and admits
  non-admin users, whatever the main URL's options say. Whether it falls back to `/api/mcp` on
  404, for a Home Assistant without per-set URLs, depends on the household's version (pending
  question 1).
- **The household token stays one, and non-admin.** Home Assistant has no per-entity permissions
  for ordinary users, so a personal token would change only whose name the logbook shows. An
  administrator household token is first reported, then refused (chunk 2).
- **Device control's read and write authority do not change.** Reads remain the allowlist plus
  the read label. Annotations are not trusted to widen the allowlist, and are not used to narrow
  it, since that would undo the read label on a script. Everyday actions keep plan 0039's tiers
  and plan 0051's proposals after outside content. Labelled devices are always proposed.
  Scheduled runs, tasks, and watches get reads only.
- **The prompt and the resource are not used.** The resource repeats the live context, and the
  prompt is server-written text in a trusted position.
- **No OAuth.** A bearer token works on every URL used here, so nothing waits for plan 0156.
- **Enabling Device control stays as it is.** The Home switch is on by default, with its
  `/privacy` line.

## Decision pending

The operator picks one scope. Chunks 1, 2, and 4 belong to every option.

- **(A) Hardening only.** Device control as today, pinned and checked. Chunks 1, 2, 2b, and 4.
  The cheapest option, and it keeps the application free of an administrator token. It does not
  give the model anything new to read or do.
- **(B) A read-only subset of the full set, behind the typed connector.** A second, separate
  administrator token, used only on `/api/mcp/homeassistant`. The connector allowlists reads by
  remote name, never `render_template`, since a template reads everything and the allowlist could
  not bound it. History is limited to exposed entities and the domains the operator adds, so
  locations stay out. Results count as outside content and are screened, because logs carry text
  from devices and the internet. The tools sit behind a "Home Assistant admin" switch that is off
  by default, for the principals the operator lists. Being reads, they reach scheduled runs and
  agent watches, so "tell me when Home Assistant logs an error" works. Chunks B1–B4.
- **(C) The full set, writes included, with its own approval story.** Option B plus
  `call_action`, offered as an always-proposed write. Every call becomes a proposal showing the
  exact action, target, and data. Templates in values are refused, because they render only at
  execution and the user would confirm something other than what runs. Integration setup flows
  and config-entry reloads are never offered. Chunks C1–C2, sketched only.

**The catalog refusing Home Assistant (chunk 3)** is part of this choice. With A or B it keeps the
model from seeing the home twice. If the operator would rather take B through the generic catalog
than through the typed connector, chunk 3 is dropped. That route is cheaper but weaker: plan
0136's key rules allow a key only over HTTPS or to a loopback address, its tools cannot filter
history to exposed entities, and an administrator token would travel through the per-user key
intake.

**Recommendation: A now. Add B only for a concrete need**, such as log alerts or history of
unexposed sensors, once the operator accepts an administrator token in the application. Not C:
a single "call any action" tool behind approval is a wide grant, and Device control plus labelled
devices already covers control.

## Approach

Tests follow the repository strategy. The connector's tests extend the existing loopback Home
Assistant fake, which serves MCP, REST, and WebSocket at the HTTP boundary, with Telegram
integration tests on a real temporary SQLite database. Each chunk leaves the full suite green.

### Before deployment (operator)

Read `/api/config` (its `version`) and the WebSocket `auth/current_user` (its `is_admin`) once,
from the operator's Mac, with the token the deployment uses. Report both before chunk 1 starts.
Each check needs only the non-admin token.

### Common chunks (every option)

1. **Device control URL.** *Complex* with the fallback, *Simple* without it. No dependencies.
   - Task: post every MCP request to `/api/mcp/assist`. With the fallback: on 404, use
     `/api/mcp`, log `homeassistant.mcp.legacy_path` once per process, and try the per-set URL
     again at the next snapshot refresh. That state is shared by snapshot refreshes, tool
     calls, approved actions, and watches.
   - Files: `internal/connectors/homeassistant/client.go`, `snapshot.go`, `service.go`,
     `service_test.go`.
   - Acceptance: against a fake serving only `/api/mcp/assist`, whose `/api/mcp` answers 403
     and lists extra tools, everything works and `/api/mcp` is never called. Live context,
     actions, approved actions, history, and watches are all covered. With the fallback, a
     fake serving only `/api/mcp` works, logs once, and moves to the per-set URL once that URL
     appears. A fake serving neither offers no home tool and the turn still works.

2. **Token and server status in `/settings`.** *Complex.* Depends on 1.
   - Task: read `auth/current_user` at each refresh. Carry two states to the Home section with
     the existing exposure check: "the token belongs to an administrator", and "Home Assistant's
     MCP server is off" (both URLs answer 404). In this chunk an administrator token only warns:
     the Home section shows the operator line, and `homeassistant.token.admin` is logged once per
     refresh that finds it.
   - Files: `internal/connectors/homeassistant/snapshot.go`, `service.go`, `service_test.go`;
     `internal/wiring/connector.go`, `wiring.go`; `internal/telegram/switch.go`,
     `switch_pages.go`, `homeassistant_integration_test.go`.
   - Acceptance: an administrator fake shows the warning and keeps every tool. A fake with no
     MCP server shows "off" instead of a refused token. A failed `auth/current_user` read
     changes nothing. A non-admin token renders exactly as today.

   2b. **Enforce.** *Simple.* Depends on 2 and on one deployed release without the warning.
   - Task: with an administrator token, offer no home tool, and end an approved home action
     without calling Home Assistant, with an outcome that names the reason.
   - Files: `internal/connectors/homeassistant/service.go`, `action.go`, `confirmed.go`,
     `service_test.go`.
   - Acceptance: as stated in the task, and the non-admin tests stay unchanged.
   - Dropped when the household runs a version with per-set URLs and chunk 1 has no fallback.
     The pinned URL then cannot widen with an administrator token, so the warning from chunk 2
     is enough.

3. **The catalog refuses Home Assistant's origin.** *Simple.* No dependencies. Pending: taken
   with A and B, dropped if B goes through the catalog.
   - Task: refuse a catalog entry whose origin (scheme, host, and port) equals the configured
     Home Assistant origin, whatever its path, when Home Assistant is configured. Entries on
     other origins are untouched.
   - Files: `internal/config/mcp.go`, `config.go`, `mcp_test.go`.
   - Acceptance: an entry on that origin stops startup with a message that names the entry and
     not its URL. An entry with `/api/mcp` on another origin loads. The example catalog loads.
     Nothing changes while Home Assistant is not configured.

4. **Documentation.** *Simple.* Depends on the chunks taken.
   - Task: update the setup steps (turn on the MCP server, through the AI card on newer versions;
     the connector uses Device control, so the main URL's options do not affect it; the token
     must belong to a non-admin user) and the pre-deploy check. Add the catalog rule if chunk 3
     is taken, and update the architecture diagram's label.
   - Files: `docs/operations.md`, `docs/architecture.md`, `README.md` if the Home line changes.
   - Acceptance: the docs match the code and name no household device, host, or network detail.

### Option B chunks

- **B1. Configuration and secret.** *Simple.* Covers `HOME_AGENT_HA_ADMIN_TOKEN` and its
  `_FILE` form, a principal list, and the extra history domains, plus the env examples and the
  Keychain and NAS secret sync. The token must differ from the household token. Startup refuses
  an admin token that is configured without a principal list.
- **B2. Full-set reads.** *Complex.* Depends on 1 and B1. A second client on
  `/api/mcp/homeassistant` and its own tool-list cache. Only allowlisted remote names whose
  annotation is `read_only` are offered: history, search, system log entries and logs, system
  health, config entries, and integration search. Tool names are prefixed `hasys_`. History
  arguments are checked against the exposed set and the configured domains. Results are
  `ThirdParty` and `Screen`. Tests use a fake full set, including a tool that turns writable and
  disappears, and a history call for a location entity that is refused.
- **B3. Switch and settings.** *Complex.* Depends on B2. A "Home Assistant admin" switch, off by
  default and shown only to listed principals, with a privacy line and a help topic, enforced
  where tools are offered and run, as plan 0136 does. Covered by a Telegram integration test that
  turns it on, runs a read in a chat turn and in an agent watch, and checks that an unlisted user
  is refused.
- **B4. Documentation.** *Simple.* Covers the administrator token's risk, which tools are
  offered, and how to revoke the token.

### Option C chunks (sketch)

- **C1. Proposed actions.** *Complex.* Depends on B2. `call_action` is offered as a write that
  always proposes. The proposal renders the action, target, and data exactly. Values containing
  templates and every `config__` tool are refused. Execution rechecks the tool list and the
  arguments.
- **C2. Documentation.** *Simple.* Covers the approval story and its limits.

## Files affected

- Common: `internal/connectors/homeassistant/`, `internal/wiring/connector.go`,
  `internal/wiring/wiring.go`, `internal/telegram/switch.go`, `internal/telegram/switch_pages.go`,
  `internal/telegram/homeassistant_integration_test.go`, `docs/operations.md`,
  `docs/architecture.md`, `README.md`.
- Chunk 3: `internal/config/mcp.go`, `internal/config/config.go`, `internal/config/mcp_test.go`.
- Option B adds `internal/config/ha.go`, `.env.example`, `deploy/nas.env.example`, the secret
  sync and deploy scripts, and the Telegram settings and help files.

## Validation

- Formatting, module, vet, build, race and non-race tests, and `scripts/check-plans`.
- The integration tests in each chunk's acceptance, against the loopback Home Assistant fake and
  the Telegram harness. No test contacts a real Home Assistant.
- Live, by the operator after deployment: a live-context question and one everyday action, with
  the log showing which URL is in use. Under B, one full-set read and one agent watch on it.

## Risks and open questions

1. **Which Home Assistant version does the household run?** A non-admin read of `/api/config`
   shows it. Recommendation: with 2026.10 or later, drop the fallback, make chunk 1 Simple, and
   drop chunk 2b. With 2026.8 or 2026.9 (per-set URLs by merge date, *inferred*), do the same
   after confirming `/api/mcp/assist` answers. With an older version, keep both.
2. **Is the deployed household token non-admin?** `auth/current_user` shows it. Recommendation:
   warn for one release, then enforce (chunk 2b), unless question 1 drops 2b.
3. **Scope: A, B, or C?** Recommendation: A now; B for a named need with an administrator token
   accepted; not C.
4. **Under B, which full-set reads, which extra history domains, and for which principals?**
   Recommendation: logs, system health, and history only, for the operator alone, with no
   location domains.
5. **Should the catalog refuse Home Assistant's origin (chunk 3)?** Recommendation: yes under A
   and B through the connector.
6. **Per-user tokens** would add only logbook attribution, at the cost of plan 0136's key intake.
   Recommendation: defer.
7. **Approval for every Device control action** would reverse plans 0039 and 0079.
   Recommendation: keep the tiers.
8. **The catalog guard compares origins.** A second name or a proxy for the same Home Assistant
   passes it, so the operations rule still applies.
9. **The connector keeps its own client.** Plan 0136's client adds protocol negotiation that
   the stateless server does not need. Moving to it stays out of scope.
