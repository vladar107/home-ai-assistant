# 0009 — Model tool calls through a core broker and Pi bridge

Status: `proposed`

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
  `-e <path>` extension still loads. `--no-builtin-tools` disables Pi's own
  tools while keeping extension tools; `--no-tools` disables both.
- The extension loader aliases `typebox` and the Pi packages, so an extension
  file outside any `node_modules` can import them. Tool arguments are
  validated by Pi against the registered JSON schema before `execute` runs.
- Tool calls and results are persisted in Pi's session file under the
  principal's agent directory.

## Goal

A conversation turn can carry the principal's tools; every tool call the
model makes is executed by the application with the principal bound from the
turn, bounded in time and size, and audited without content. Turns without
tools keep today's lockdown exactly.

## Decisions

- Add a core broker package, `internal/tools`, that aggregates tool
  contributions from connector modules. `ToolsFor(principal)` returns the
  definitions and prompt guidance the principal is granted; `Execute`
  validates the tool name against that grant again, runs it with a 15-second
  deadline, caps the result at 16 KiB, limits a turn to 8 calls, prefixes
  every result as untrusted data, and logs principal, tool, outcome,
  duration, and result size only. The principal comes from the turn; no
  argument can name a principal, credential, or base URL.
- Bridge tools into Pi with an application-owned extension. The extension is
  a small TypeScript file embedded in the binary and written per turn, with a
  `tools.json` of the turn's definitions, into the turn's private working
  directory. Pi loads it with `-e` while `--no-extensions` stays; a turn with
  tools uses `--no-builtin-tools`, a turn without tools keeps `--no-tools`
  and writes nothing. The extension registers exactly the listed definitions
  and forwards each call to the application.
- Transport is a per-turn HTTP listener on `127.0.0.1:0` with a per-turn
  random bearer token; address and token are passed only in the Pi process
  environment and the listener closes with the turn. A Unix socket was
  rejected because macOS temporary directories exceed the socket path limit;
  stdio was rejected because that stream is Pi's RPC channel.
- The runtime contract gains a tool executor next to `Tools` on the run
  request. Pi-specific event names, the extension, and the listener stay in
  the adapter; the adapter allows tool events when tools are present and
  reports tool names only through the event sink and logs.
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
   grant recheck, limits, and audit logging. Complexity: complex.
   Prerequisite for the rest.
2. After step 1, Pi adapter: embedded extension, per-turn working-directory
   files, listener and token, argument and environment changes, prompt
   composition, tool event handling, and the fixture scenarios below.
   Complexity: complex. Owns `internal/runtime/pi/` and the fixture.
3. After step 1, coordinator wiring: the broker replaces `NoTools`, the
   executor is bound to the turn's principal, and per-turn call budget
   resets. Complexity: moderate. Owns `internal/conversation/` and
   `internal/app/`.
4. Documentation: README conversation section and the Pi lockdown note in
   `docs/04-integrations.md`.

## Files affected

- `internal/runtime/runtime.go`, `internal/runtime/pi/`, and an embedded
  extension file
- new `internal/tools/`
- `internal/conversation/`, `internal/app/`, `internal/testutil/pifixture/`
- `README.md`, `docs/04-integrations.md`

## Testing

Integration suites over real components, following plan 0003; no Node or
network access in routine tests.

### Pi adapter

- Extend the fixture with a `fixture:tool <name> <json>` scenario that reads
  `tools.json` and the bridge address from its arguments and environment,
  posts the call the way the extension does, and echoes the returned text
  into its response. Assert that a turn with tools starts the fixture with
  `--no-builtin-tools` and `-e` naming the bridge file in the working
  directory, without `--no-tools`; that `tools.json` holds exactly the
  turn's definitions; that the fixture's call reaches the executor with the
  turn's principal and returns its result; that a wrong bearer token, an
  unknown tool name, an oversized result, and a call after the per-turn
  budget are rejected without reaching the executor or aborting the turn;
  and that the listener refuses connections once the turn ends.
- Assert that a turn without tools keeps `--no-tools`, sets no bridge
  environment, writes no bridge files, and still aborts on a tool event.
- Owns `internal/runtime/pi/*_test.go`.

### Coordinator

- With a controllable provider, assert that the turn passes the principal's
  definitions and guidance to the runtime, that a provider error yields the
  unavailable reply, and that a principal without contributions runs a
  tool-less turn.
- Owns `internal/telegram/*_test.go`.

## Validation

- All suites pass under `go test ./...` and `go test -race ./...`, with the
  configured formatting, module, vet, build, and plan checks.
- Live verification of the real extension inside Pi is performed with the
  first connector in [0010](0010-notion-workspace-connection.md); this plan
  alone adds no user-visible tool.
- Coordinate with the operator's GoLand process; do not start a second
  Telegram poller.

## Risks and open questions

- The TypeScript extension is exercised only live; keep it tiny and pin its
  behaviour to the Pi version check already in place. A deterministic check
  against a loopback model endpoint can be added later if it drifts.
- Tool calls extend a turn; the turn timeout still bounds the whole turn, so
  a long tool chain may need a larger `HOME_AGENT_CONVERSATION_TURN_TIMEOUT`.
- Pi session files now contain tool arguments and results under the
  principal's directory; `/new` rotates the session but does not delete them.
