# 0010 — Per-user Notion workspace connection

Status: `done`

## Context

The bot has no integration yet. `docs/04-integrations.md` selects a private
Notion connection per principal, with explicitly shared pages as the content
boundary, and the product direction requires each user to discover, connect,
and disconnect their own integrations. Notion offers no device-code flow and
its OAuth flow needs a public redirect endpoint, which the deployment
excludes, so the credential is a bearer token the user copies from Notion's
developer portal.

Verified against Notion's API documentation on 2026-09-06:

- The current `Notion-Version` is `2026-03-11`; databases are containers of
  data sources since `2025-09-03`, search returns pages and data sources,
  and `POST /v1/data_sources/{id}/query` queries one data source.
- An internal connection is created by a workspace owner, sees only pages
  shared with it, and has capabilities configured per token: read content,
  update content, insert content, comments, and user information. Its token
  is static and is refreshed from the connection's configuration tab.
- A personal access token acts as its creator with everything that person
  can access, cannot be restricted below full API access, expires after at
  most one year, and can be revoked immediately. Notion recommends it for
  personal workflows and connections for automations that should not depend
  on one person's permissions.
- `GET /v1/users/me` validates a token and returns the bot's name, workspace
  ID, and workspace name; `bot.owner.type` is `workspace` for an internal
  connection and `user` for a personal access token; an invalid token
  returns `401 unauthorized`.
- The per-connection rate limit averages three requests per second; `429`
  and `529` carry `Retry-After`. Errors carry `code` and `message`.

## Goal

Each authorized user connects their own Notion token, the model can search,
read, and query what that user shared with it, and the user can see and
remove the connection. No user can reach another user's workspace, and the
token never appears in Telegram replies, logs, SQLite, or the model.
Write tools are a separate plan,
[0011](0011-confirmed-notion-writes.md); this plan classifies every tool so
that plan can gate them.

## Decisions

- The recommended credential is an internal connection with read, update,
  and insert content capabilities, shared only with the pages and data
  sources the user wants. A personal access token is accepted for a user
  who cannot create a connection; the app records the token kind from
  `users/me` and the settings section states that a personal token grants
  everything the user can access and expires.
- Every contributed tool carries a risk class, `read` or `write`. This plan
  contributes the read tools; the broker offers a principal only the classes
  it is granted, and a connection grants `read` alone until the writes
  switch of plan 0011 is on. A reconnect resets the writes flag.
- Two provisioning paths write the same file,
  `data/integrations/<principal>/notion.json`, mode 0600 inside 0700
  directories, written atomically like the model credentials. The file
  layout and the non-secret connection record live in a core package,
  `internal/integration`, shared by later connectors. The operator path
  installs the file from standard input with
  `scripts/notion-set-token <principal>`, locally or through the SSH alias
  on the NAS as the runtime user that owns the data directory; the app
  adopts such a file by verifying it on the next `/settings` or turn and
  creating the row. The chat path is enabled by
  `HOME_AGENT_NOTION_CHAT_CONNECT` and can stay off.
- Chat intake happens only on the deterministic path. The Connect Notion
  button in `/settings` starts a memory-only intake for that principal that
  expires after ten minutes; the next message from that principal is
  consumed and is never enqueued for the model. A token-shaped message is
  deleted with `deleteMessage` before verification; if deletion fails, the
  bot continues and asks the user to delete the message themselves. A
  message that is not token-shaped ends the intake with guidance and is
  left in place.
- An ordinary message that looks like a Notion token (`ntn_` or legacy
  `secret_` followed by alphanumerics) is deleted and answered with
  guidance instead of being enqueued, whether or not chat intake is on.
- A token is stored only after `GET /v1/users/me` succeeds. Non-secret
  metadata goes to a new `integration_connections` table: principal,
  integration, token kind, workspace ID, workspace name, bot name, writes
  flag (off, used by plan 0011), connected timestamp, primary key on
  principal and integration.
- Connector `internal/connectors/notion`: typed client with a ten-second
  request deadline, one retry on `429` or `529` honouring `Retry-After` up to
  five seconds, page size capped at 25, and normalized error classes
  (unauthorized, restricted, not shared, rate limited, invalid,
  unavailable). Responses are rendered to compact text with titles, IDs,
  URLs, last-edited times, and property values; a data source lists its
  property names and types; page content is flattened to plain text up to
  12 KiB so the rendered page stays below the broker's 16 KiB result cap.
  No response content is logged; failures log the error class only.
- Read tools contributed to the broker of
  [0009](0009-model-tool-calls-through-pi.md): `notion_search` (query,
  object type, limit, cursor), `notion_get_page` (page ID, content included
  unless `include_content` is false), and `notion_query_data_source` (data
  source ID, optional filter and sorts passed through for Notion to
  validate, limit, cursor). Only a principal with both the file and the row
  receives them; the broker rechecks before each execution and the connector
  reads the token fresh per call, so disconnecting mid-turn makes the next
  call fail closed.
- The connection is managed from the `/settings` message of
  [0007](0007-settings-command.md). A Notion section shows the connection,
  token kind, and writes state or how to connect, with a Connect button when
  chat intake is on and a Disconnect button when connected. A token file
  Notion rejects or cannot verify is reported as such, left in place, and
  removable with Disconnect. No new command is registered.
- Configuration: `HOME_AGENT_NOTION_ENABLED` (default false, requires
  conversation), `HOME_AGENT_NOTION_CHAT_CONNECT` (default false), and
  `HOME_AGENT_NOTION_API_BASE_URL` for loopback tests. The NAS Compose file
  forwards the two switches from `runtime.env`, both off by default.
- The connector registers as the first connector module in the registry with
  ID `notion`; the application registers it during initialization, after the
  store opens, and the module order is settled after connectors register. It
  contributes tools and lifecycle only.
- Notion content read through tools reaches the user's own model
  subscription; this is inherent to the feature and is stated in the
  connect instructions and `/privacy`.

## Approach

1. Two parallel work areas.
   - Connector: client, rendering, error normalization, and a loopback
     simulator of `users/me`, `search`, `pages`, `blocks/children`, and
     `data_sources/query`. Owns `internal/connectors/notion/` and
     `internal/testutil/notionsim/`.
   - Platform: `integration_connections` migration and store methods,
     credential file handling in `internal/integration/`, the install
     script, and `deleteMessage` in the Telegram client and simulator.
2. After step 1, the Notion service: adoption of operator-placed files,
   connect, status, disconnect, intake state with expiry, token guard, the
   tool provider with risk classes, and the Notion settings section with a
   message interceptor in the Telegram service.
3. Wiring: configuration, module registration, application start and
   shutdown, `.env.example`, README, runbook, and the suites below.

## Files affected

- new `internal/connectors/notion/`, `internal/integration/`, and
  `internal/testutil/notionsim/`
- `internal/store/sqlite/` and migration `006_integration_connections.sql`
- `internal/telegram/`, `internal/config/`, `internal/app/`
- new `scripts/notion-set-token`
- `.env.example`, `deploy/compose.app.yaml`, `deploy/nas.env.example`,
  `README.md`, `docs/04-integrations.md`, `docs/13-operator-runbook.md`

## Testing

Integration suites over the loopback Telegram simulator, a loopback Notion
simulator, real SQLite, and the controllable runtime, following plan 0003.

### Connection lifecycle

- An operator-placed file is adopted on `/settings`: verified against the
  Notion simulator exactly once, the row is created, and the section names
  the workspace, bot, and token kind; an invalid file is reported and left
  in place without a row.
- With chat intake on, the Connect Notion button replies with the
  instructions; the next message is deleted through the simulator,
  verified, stored as a 0600 file inside 0700 directories with the row
  present, and answered with the workspace and bot name. A rejected token is
  deleted and answered with nothing stored. A non-token message ends the
  intake without deletion or a model turn. An expired intake treats the next
  message as conversation. A second press replaces the pending intake. A
  failed deletion still connects and asks the user to delete the message.
  With chat intake off, the section has no Connect button and explains the
  operator path.
- A token-looking ordinary message is deleted, answered, and not enqueued.
  `/privacy` states that Notion content reaches the model. The Disconnect
  Notion button removes the file and the row and re-renders the settings
  message.
- Logger output, SQLite contents, and every sent message never contain a
  token.
- Owns `internal/telegram/notion_integration_test.go`.

### Tool execution and isolation

- A connected principal's turn carries the three read definitions and
  guidance; executing each tool through the turn's executor reaches the
  simulator with that principal's bearer token only, renders titles, IDs,
  properties, schema, content, and parent, passes a filter through, and
  reports a malformed filter and an unshared page as content-free tool
  errors. A second principal without a connection runs a tool-less turn, and
  connecting the second principal never uses the first principal's token.
  Disconnecting during a turn makes the next execution fail closed without
  a request to the simulator.
- Owns `internal/telegram/notion_integration_test.go`.

### Connector

- Page size cap, cursor handling, `429` retry with `Retry-After`, no retry
  past the allowance or without it, `401`, `403`, `404`, and `5xx`
  classification, result byte cap with the truncation marker, block
  flattening, properties-only rendering, rejected connects storing nothing,
  and no response content or token in logs.
- Owns `internal/connectors/notion/service_test.go`.

## Validation

- All suites pass under `go test ./...` and `go test -race ./...`, with the
  configured formatting, module, vet, build, and plan checks.
- Live, from the operator's GoLand run: install a connection token with the
  script, confirm `/settings` adopts it; ask for a search, a page, and a
  data source query and confirm the model used the tools; if chat intake is
  on, connect through the button and confirm the token message disappears;
  disconnect from `/settings`; confirm the token is absent from logs and the
  database. The runbook lists these steps; they were not yet performed when
  this plan was committed.
- Coordinate with the operator's GoLand process; do not start a second
  Telegram poller.

## Risks and open questions

- With chat intake, Telegram's servers see the token message before
  deletion, and a notification preview on another device may have shown it.
  Rotation is a refresh in Notion followed by reinstalling the token.
- Operator provisioning means a second household user hands their token to
  the operator; the chat path exists for that reason.
- `users/me` does not report a token's capabilities; a connection created
  without read content surfaces as a restricted error on first use.
- Notion pages are untrusted content; the broker labels results as data and
  these tools are read-only, but the model still reads what the pages say.
- Filters passed through to the data source query are validated by Notion,
  not locally; malformed filters surface as a tool error to the model.
- Multiple connections per user, polling, and briefing use of Notion are
  later plans.
