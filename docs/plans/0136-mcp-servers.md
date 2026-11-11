# 0136 — MCP servers from an operator catalog

Status: `done`

## Context

Every integration so far is a connector written for one service. A user who wants the
assistant to look somewhere new, such as a marketplace or a price tracker, waits for a
connector. Many such services already publish a Model Context Protocol (MCP) server,
hosted or as a small local program.

The pieces an MCP integration needs already exist: the tool broker offers typed tools
per principal and rechecks the grant before each call, integrations have per-user
switches and per-principal credential files, third-party results are marked and
screened, and an agent watch runs a read-only model turn on an interval and alerts
when a condition holds. The Home Assistant connector already speaks a minimal,
sessionless MCP over HTTP. What is missing is a general client and a way to offer the
tools of servers the operator chose.

## Goal

The operator lists MCP servers in a catalog file. Each authorized user sees them in
`/settings`, turns on the ones they want, and, for a server that needs a key, connects
their own. The server's allowlisted read tools are then offered in that user's chat
turns, scheduled runs, background tasks, and agent watches, so "tell me when this
drops below a price" works through an agent watch with no new watch code.

Success: with one shared server and one keyed server configured, two users each get
only what they turned on or connected, a user's key is never sent for another user,
and an agent watch can call the tools.

## Decisions

- **Operator catalog, not user-added servers.** Servers come from a JSON file named by
  `HOME_AGENT_MCP_SERVERS_FILE`; without it the feature is off. An entry has a name,
  a title, a one-line description, a URL, an auth kind, the allowlisted tools, an
  optional guidance line, and an optional list of principals who may use it. A name
  matches `^[a-z][a-z0-9]{0,15}$`. A catalog holds at most 12 servers and a server at
  most 16 tools. A URL carries no user info, query, or fragment.
- **Streamable HTTP only.** The application never starts a server process. A server
  that only speaks stdio runs as its own container behind a stdio-to-HTTP proxy, with
  no access to the application's data or secrets. That container is the operator's to
  build and pin, in a project outside this repository.
- **Auth kinds `none` and `key`.** A `none` server is shared: one endpoint, no
  credential. A `key` server takes each user's own key, sent in the header and with
  the prefix the entry names (`Authorization` and `Bearer ` by default); the header
  name and prefix hold no line breaks, and the name is not a hop-by-hop, `Host`, or
  `Content-*` header. A `key` server must use HTTPS, except on a loopback address, so
  a sidecar reached over plain HTTP is always a `none` server. OAuth is left to a
  later plan; the auth kind leaves room for it.
- **Per-user keys in the existing store, bound to the server's origin.** A key lives
  in `data/integrations/<principal>/mcp-<server>.json` through
  `integration.Credentials`. Its non-secret record reuses `integration_connections`
  as a personal token whose workspace ID is the origin of the server's URL, so no
  migration is needed. A key whose recorded origin differs from the catalog's is not
  sent: the server shows as needing reconnection. There is no household or fallback
  key. A key is never logged, traced, exported, or shown to the model, and the
  message that carried it is removed.
- **Key intake is narrow.** One intake is pending per principal at a time and names
  its server. The next message is taken as the key only when it is a single token of
  16 to 512 characters without whitespace and matches the entry's optional
  `key_pattern`; any other message ends the intake and is sent nowhere outside.
  A consumed message is deleted and never becomes a turn, also when its update is
  delivered again, arrives in a burst, or is a caption. Another connector's secret
  ends the intake instead of being sent as a key, and a message shaped like a
  declared `key_pattern` is kept from the model even with no intake pending.
- **Verifying a key is best effort.** Connect runs `initialize` and `tools/list` with
  the key and refuses it on HTTP 401 or 403. A server may accept those and still
  refuse a call; a refused call marks the connection as a problem in `/settings`.
- **A switch per server, off by default.** Each server is an integration named
  `mcp-<server>` with the usual per-user switch. Off is the default because turning
  one on sends what the user asks to a service outside the household. A `key` server
  additionally offers nothing until the user connected a key; connecting a key turns
  that server on, since pasting a key is consent to use it, and Disconnect removes
  the key and turns the switch off.
- **The principal list is enforced where tools are offered and run,** not only in
  `/settings`, so a restored switch cannot grant a server to a user it is not for.
- **Reads only, and only what the catalog names.** A tool is offered only when the
  catalog allowlists its remote name; every offered tool is `RiskRead`. Tools the
  server lists beyond that are ignored, and so are the server's own annotations and
  its `instructions`. Because they are reads, they reach scheduled runs, tasks, and
  agent watches through `ReadOnlyToolsFor` unchanged.
- **Results are outside content.** Every tool is `ThirdParty` and `Screen`, so a call
  marks the conversation exposed and its result is screened like a web page. Text
  content is returned, clipped by the connector to the broker's result cap; other
  content kinds are dropped and named.
- **Tool names are `mcp_<server>_<tool>`,** normalized to the broker's name pattern,
  so no server can collide with a built-in tool. Tools are offered in catalog order.
- **A server cannot break a turn.** The connector checks each mapped definition
  (name and length, a non-empty description, a schema that is a plain JSON object
  within a size cap) and drops the ones that fail, including two that normalize to
  the same name. Descriptions come from the server, clipped, unless the entry pins
  one. Contributing tools never returns an error.
- **A server cannot slow every turn.** A tool list is cached for five minutes per
  server and credential. A refresh has three seconds; on failure the previous list is
  served for up to an hour, and a server with none is skipped and retried with
  backoff, as the Home Assistant snapshot does. A healthy server nobody used for
  over an hour is waited for, together with every other such list of that user,
  so the turn waits about one refresh in total.
- **The client asks for nothing back.** It advertises no client capabilities and
  declines sampling, elicitation, and roots requests. It keeps one session per
  principal and server, so a stateful shared server cannot mix users, and ends
  sessions on shutdown as best effort. It does not follow redirects, so a key cannot
  be carried to another host.
- **A hand-written client, no new dependency.** Only `initialize`, `tools/list`, and
  `tools/call` are needed. The Home Assistant connector keeps its own sessionless
  client; unifying them is out of scope.
- **One settings section for all servers.** `/settings` gains a "Services" group with
  one section that shows a row per server the user may use and carries the server's
  name in its actions, so the fixed section, order, and privacy lists gain one entry
  each however many servers there are.

## Approach

1. **Catalog.** Load and validate the catalog in `internal/config`, following the NAS
   roots file. An invalid catalog stops startup. The example catalog is loaded by a
   test.
2. **Client and simulator.** A client in `internal/connectors/mcp` for streamable
   HTTP: `initialize` with protocol-version negotiation, an optional session ID with
   one re-initialize when the server forgets it, paginated `tools/list`, `tools/call`,
   JSON or event-stream answers, bounded sizes and deadlines. Read the current and the
   previous transport revisions of the specification first; proxied stdio servers
   speak older ones. Every request accepts both JSON and event-stream answers; in a
   stream the client skips other events and takes the response with its request's ID.
   A server may be stateless: no session ID, an empty answer to the initialized
   notification. The client sends a protocol version from the revisions that have the
   `initialize` handshake. A result's structured content is ignored beside its text.
   A loopback simulator in `internal/testutil/mcpsim` stands in for
   servers in every test.
3. **Connector.** One `tools.Provider` per catalog server, each `integration.Switched`.
   It keeps the tool list cache, maps allowlisted tools to definitions, runs calls,
   and owns Connect, Disconnect, and Status for keyed servers. It adds the activity
   key and label its tools carry.
4. **Settings section.** The Services section in `internal/telegram`, written against
   a small controller interface: rows, on and off, key intake, `/privacy` lines for
   servers that are on, and an `app_help` topic.
5. **Wiring.** Build the connector from the catalog, register providers and switches,
   hand the section its controller, and cover the whole path end to end.
6. **Deployment and documentation.** A compose overlay that mounts the catalog and
   joins an external network the sidecar project creates, applied by the deploy script
   while the catalog is configured; the new settings in the local env files;
   architecture and operations documentation, including a hardened sidecar example,
   the rule for what may be allowlisted, and removing a server's leftover key files.

A failed call returns an error result the model can adapt to, with no server text in
application logs. Telemetry reuses the broker's per-tool counters and traces.

## Files affected

- `internal/config/mcp.go`, `internal/config/config.go`, tests
- `deploy/mcp-servers.example.json`, `.env.example`, `deploy/nas.env.example`, `.gitignore`
- `internal/connectors/mcp/` (client, snapshot, service, tools), tests
- `internal/testutil/mcpsim/`
- `internal/tools/broker.go` (activity key), `internal/conversation/activity.go`
- `internal/telegram/` (services section, settings groups and orders, privacy, help),
  the integration test harness, integration tests
- `internal/wiring/wiring.go`, `internal/wiring/export.go` (a comment), `internal/app/app.go`
- `deploy/compose.app.mcp.yaml`, `scripts/deploy-app-nas`
- `docs/architecture.md`, `docs/operations.md`, `README.md`

## Validation

- Formatting, module, vet, build, race and non-race tests, and `scripts/check-plans`.
- Catalog tests for every rule above, and the example catalog loads.
- Integration tests against the simulator: two users with different keys never see
  each other's key on the wire; a switched-off or unconnected server offers nothing; a
  user outside the principal list gets nothing even with the switch on; an unlisted
  tool is neither offered nor callable; a tool named like a built-in one, a malformed
  definition, a down server, and a hanging server each leave the turn working; session
  expiry recovers once; an oversized or non-text result is bounded; a redirect is
  refused; a key is not sent after the catalog's origin changed; the key appears in no
  captured log line.
- End to end through the Telegram simulator: connect by pasting a key, a message that
  is not a key reaches no server, a chat turn calls a tool and the conversation becomes
  exposed, a scheduled read-only run and an agent watch get the tool, disconnect
  removes the key file, an exported switch restores, and a full catalog of 12 servers
  renders.
- Live, by the operator after deployment: one shared sidecar server and an agent watch
  on a price.

## Risks and open questions

- **Tool descriptions are server-written text in a trusted position.** A hostile or
  compromised server could instruct the model through them. The catalog limits which
  servers and tools exist, descriptions are clipped, and an entry can pin its own; a
  changed description is not detected in this version, and it also invalidates the
  cached prompt.
- **Arguments leave the household.** After outside content, a tool call can carry
  conversation text to the server, as a web search query can. The destination is fixed
  by the catalog, so the rule is operational: never allowlist a tool that takes an
  address or fetches on the caller's behalf. Checking arguments as web search queries
  are checked is a possible follow-up.
- **A sidecar is third-party code on the NAS.** It mounts none of the application's
  data or secrets, but anything on the network the application joins can reach the
  application's own listener, so that network holds only relays the operator
  controls and never the server itself. Container networking alone does not keep a
  sidecar off the local network; that restriction belongs to the operator's network
  configuration.
- **Unusual schemas.** A server's tool schema the runtime cannot validate could fail a
  run; the connector's schema check is conservative, and what the runtime accepts is
  confirmed during implementation.
- **Leftovers.** Removing a catalog entry leaves its users' key files and switch rows;
  cleanup is a documented operator step. Until then a leftover or restored switch
  applies again to a server later added under the same name.
- **Unofficial servers break.** A community server that reads a site's own endpoints
  stops working when the site changes; the assistant then reports a failed call.
- **Specification drift.** The transport and authorization parts of the specification
  are still changing; the client negotiates the version and is covered by simulator
  tests for the revisions it supports.
- **Later plans:** OAuth for servers that demand it, confirmed write tools, and a way
  for a user to ask the operator for a new server.
