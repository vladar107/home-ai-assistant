# 0009 — Model tool calls through a core broker and Pi bridge

Status: `done`

## Context

The conversation path passes no tools: the coordinator uses `NoTools`, the Pi
adapter rejects any request that carries a tool definition, starts Pi with
`--no-tools`, aborts the turn on any tool event, and the system prompt tells
the model it has no integrations. The first useful integration,
[0010](0010-notion-workspace-connection.md), needs the model to call typed
tools that the Go application executes under the requesting principal's
grants, as described in the tool execution policy of
`docs/05-security-and-privacy.md` and the module contract of
`docs/12-module-and-automation-model.md`.

Verified against the pinned Pi 0.84.1 package on 2026-09-06:

- RPC mode has no host-side tool protocol. Tools exist only when an extension
  running inside the Pi process registers them with `pi.registerTool()`.
- `--no-extensions` disables extension discovery only; an explicit
  `--extension <path>` still loads. `--no-builtin-tools` disables Pi's own
  tools while keeping extension tools; `--no-tools` disables both.
- The extension loader aliases `typebox` and the Pi packages, so an extension
  file outside any `node_modules` can import them. Tool arguments are
  validated by Pi against the registered schema before `execute` runs, and a
  plain JSON Schema object is accepted without TypeBox.
- An error thrown by a tool's `execute` becomes an `isError` tool result for
  the model, not an `extension_error` event.
- With a custom `--system-prompt`, Pi appends no tool snippets of its own;
  tool descriptions reach the model only as function definitions.
- Tool calls and results are persisted in Pi's session file under the
  principal's agent directory.
- Since [0015](0015-resumable-pi-sessions.md) the working directory is a
  stable `workspace` directory per principal, so per-turn files cannot live
  there.

## Goal

A conversation turn can carry the principal's tools; every tool call the
model makes is executed by the application with the principal bound from the
turn, bounded in time and size, and audited without content. Turns without
tools keep today's lockdown exactly.

## Decisions

- Add a core broker package, `internal/tools`, that aggregates tool
  contributions from connector modules. `ToolsFor(principal)` returns the
  definitions, the prompt guidance, and an executor bound to that principal
  for one turn; the executor validates the tool name against the grant again
  before every call, runs it with a 15-second deadline, rejects a result above
  16 KiB, limits a turn to 8 calls, prefixes every result as untrusted data,
  and logs principal, tool, outcome, duration, and result size only. Every
  definition carries a `read` or `write` risk class for
  [0011](0011-confirmed-notion-writes.md) to gate. The principal comes from
  the turn; no argument can name a principal, credential, or base URL.
- Bridge tools into Pi with an application-owned extension. The extension is
  a small TypeScript file embedded in the binary and written per turn, with a
  `tools.json` of the turn's definitions, into a private `bridge-*` directory
  created inside the principal's agent directory and removed when the turn
  ends. Pi loads it with `--extension` while `--no-extensions` stays; a turn
  with tools uses `--no-builtin-tools`, a turn without tools keeps
  `--no-tools` and writes nothing. The extension registers exactly the listed
  definitions and forwards each call to the application.
- Transport is a per-turn HTTP listener on `127.0.0.1:0` with a per-turn
  random bearer token; the address, the token, and the `tools.json` path are
  passed only in the Pi process environment and the listener closes with the
  turn. The listener rejects a wrong token, a malformed body, and arguments
  above 64 KiB before the executor runs. A Unix socket was rejected because
  macOS temporary directories exceed the socket path limit; stdio was
  rejected because that stream is Pi's RPC channel.
- The runtime contract carries a `ToolSet` on the run request: definitions,
  guidance, and the executor. Pi-specific event names, the extension, and the
  listener stay in the adapter; the adapter allows tool events when tools are
  present and reports tool names only through the event sink. A turn with
  tools spans several assistant messages around tool calls; their texts are
  joined by a blank line.
- The adapter composes the system prompt per turn: the base prompt plus the
  broker's guidance for the granted tools, or the current "no integrations"
  sentence when the turn has none.
- Connector modules contribute tools through a narrow provider interface
  registered at wiring time; they never receive the application, the store,
  or a path to Pi.
- Persisted tool results in Pi session files share the boundary of the
  conversation history already stored there; documented, not changed.

## Approach

1. Broker package and runtime contract: tool definition, provider, executor,
   grant recheck, limits, and audit logging.
2. Pi adapter: embedded extension, per-turn bridge directory, listener and
   token, argument and environment changes, prompt composition, tool event
   handling, and the fixture scenario below.
3. Coordinator wiring: the broker replaces `NoTools`, the executor is bound to
   the turn's principal, and the per-turn call budget lives in the executor.
4. Documentation: README conversation section and the Pi lockdown note in
   `docs/04-integrations.md`.

## Files affected

- `internal/runtime/runtime.go`, `internal/runtime/pi/`, and the embedded
  `bridge.ts`
- new `internal/tools/`
- `internal/conversation/`, `internal/app/`, `internal/testutil/pifixture/`
- `README.md`, `docs/04-integrations.md`

## Testing

Integration suites over real components, following plan 0003; no Node or
network access in routine tests. The broker is exercised through the adapter
and coordinator suites with the real broker and small in-test providers.

### Pi adapter

- A `fixture:call <name> <json>` scenario reads the bridge address and token
  from its environment, posts the call the way the extension does between
  two assistant messages with Pi's tool events, and echoes the returned text.
  Asserts that a turn with tools starts the fixture with `--no-builtin-tools`
  and `--extension` naming the bridge file in a per-turn directory under the
  agent directory, without `--no-tools`; that `tools.json` holds exactly the
  turn's definitions; that the call reaches the provider with the turn's
  principal and its result comes back marked untrusted; that the system
  prompt carries the guidance; that the sink receives the tool name; and that
  the bridge directory and listener are gone once the turn ends.
- During a held turn, a missing or wrong bearer token, oversized arguments,
  and a malformed body are rejected by the listener; an unknown tool name
  and a call past the per-turn budget are rejected by the broker without
  reaching the provider; an oversized result and a failing provider produce
  content-free tool errors; none of them aborts the turn.
- A request with definitions but no executor never starts a process, and a
  turn without tools keeps `--no-tools`, sets no bridge environment, writes
  no files, and still aborts on a tool event.

### Coordinator

- With a registered provider, a turn carries the principal's definitions,
  guidance, and a working executor; a second principal without a
  contribution runs a tool-less turn; the audit log names principal, tool,
  and outcome without content; and a failing contribution yields the
  unavailable reply with the `tool_provider` error kind.

## Validation

- All suites pass under `go test ./...` and `go test -race ./...`, with the
  configured formatting, module, vet, build, and plan checks.
- The embedded extension was loaded by the pinned local Pi 0.84.1 in RPC mode
  with `--no-builtin-tools --extension` and a `tools.json`; a probe extension
  reported the listed tool as registered and active. Live use of a real tool
  is performed with the first connector in
  [0010](0010-notion-workspace-connection.md); this plan alone adds no
  user-visible tool.
- Coordinate with the operator's GoLand process; do not start a second
  Telegram poller.

## Risks and open questions

- The TypeScript extension is exercised live only at load time; keep it tiny
  and pin its behaviour to the Pi version check already in place. A
  deterministic check against a loopback model endpoint can be added later if
  it drifts.
- Tool calls extend a turn; the turn timeout still bounds the whole turn, so
  a long tool chain may need a larger `HOME_AGENT_CONVERSATION_TURN_TIMEOUT`.
- Pi session files now contain tool arguments and results under the
  principal's directory; `/new` rotates the session but does not delete them.
- jiti compiles the extension on every turn and may keep a compile cache in
  the system temporary directory; the file is identical across turns.
