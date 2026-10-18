# 0105 — The model-facing prompt scaffolding in one place

Status: `done`

## Context

An architecture audit found that the text the model reads around the
connectors' own tools is spread over many packages:

- The system prompt's fixed sections and its assembly live in
  `internal/runtime/pi`, beside the Pi process handling.
- The `[system]` lines a turn's message starts with are built in
  `internal/conversation` (the forwarded-message notice, the tool hint, the
  notice, the place and user lines, the subject line, the confirm
  continuation, and the 👎 reconsider message) and in `internal/localtime`
  (the current-time line).
- The framing of turns nobody is watching is written separately in
  `internal/schedule` (scheduled jobs), `internal/trip` (trip scans),
  `internal/watch` (agent watch checks), and `internal/task` (background
  tasks), with the same sentences copied between them.
- What the broker answers a call it does not pass to a tool (budget,
  unavailable, timeout, failure, size) and how it frames a result as
  untrusted live in `internal/tools`, and what a write call answers once
  approvals take it lives in `internal/approval`.

Tuning passes have had to chase wording across these packages, and nothing
pins the system prompt's exact bytes. The system prompt must stay cacheable,
so per-turn facts such as the time go in the message, never the prompt, and
a change that moved one into the prompt would go unnoticed.

## Goal

One leaf package, `internal/prompt`, holds the cross-cutting prompt
scaffolding, the call sites use it, and golden files pin the exact text for
representative tool sets and turns. The model receives byte-identical text
before and after the move.

## Decisions

- `internal/prompt` imports no domain package. It depends only on
  `internal/localtime` for the shared time format, a leaf utility. Callers
  pass in names, ids, times, zones, and the turn's guidance.
- `prompt.System(hasTools, guidance, webSearch)` takes nothing that changes
  from turn to turn, so a per-turn fact cannot reach the system prompt
  without a signature change. `prompt.WithSections` appends memory and jobs
  as before. A test also refuses a `[system]` marker in any pinned system
  prompt.
- The `[system]` line builders return an empty line for an empty value, and
  `prompt.Prepend` leaves the message as it is for an empty line, so the
  call sites keep their order without their own emptiness checks.
- The unattended framings share their common sentences as constants inside
  the package, so a wording change reaches every unattended turn together.
- `conversation.NothingToAdd`, `ForwardedNotice`, `ContinueMessage`, and
  `ReconsiderMessage` move to `prompt.NothingToAdd`, `Forwarded`, `Continue`,
  and `Reconsider`. Their callers, tests included, use the new names.
  `localtime.Line` becomes `prompt.Time`.
- `approval.ProposedText` stays as a name for `prompt.Proposed`, because it
  is part of the API connectors return and compare against. Moving every
  connector to the new name would touch many connector files for no change
  in behaviour.
- The history reader strips paragraphs by `prompt.Marker` instead of its own
  copy of `[system]`.
- Golden files under `internal/prompt/testdata` hold the exact bytes; `go
  test ./internal/prompt -update` rewrites them after an intended change.

What deliberately stays where it is:

- Connector and module tool descriptions and guidance, including the
  scheduled jobs', watches', trips', memory's, and the react tool's, stay
  with their connectors. They are per-tool and change with the tool.
- The memory and jobs sections stay rendered by `internal/memory` and
  `internal/schedule`, since they render the user's own data; the prompt
  package only places them.
- The location and trip lines' contents stay in `internal/location` and
  `internal/trip`; `prompt.Place` only marks them.
- Reply contracts that a parser in the same file reads stay beside that
  parser: the agent watch check's JSON verdict, the trip scan's task, and
  the tab-name request. The rewrite request is a single-purpose utility
  prompt and stays with it.
- Approval's record-state reports (the confirm, reject, expire, and Undo
  notices, the status lines shared with the user's messages, and the
  refusal errors) stay in `internal/approval`; they report its records
  rather than steer the model.
- The Heads-up job's stored prompt stays in `internal/schedule`; it is job
  content, not scaffolding.

## Approach

1. Capture the old text for representative inputs from every moved site.
2. Add `internal/prompt` with the system prompt, the turn lines, the
   unattended framings, and the tool and act results.
3. Point the runtime, conversation, history, schedule, trip, watch, task,
   tools, approval, reaction, and telegram call sites at it, and remove the
   old copies.
4. Compare the captured text with the new output, then pin it in golden
   files with a test in the package.
5. Name the package in the README layout and in the architecture's turn
   flow.

## Files affected

- `internal/prompt/` (new) and its `testdata/`
- `internal/runtime/pi/runtime.go`
- `internal/conversation/service.go`, `scheduled.go`, `reaction.go`,
  `types.go`, `history/history.go`
- `internal/localtime/localtime.go`
- `internal/schedule/modelturn.go`, `headsup.go`
- `internal/trip/scan.go`, `internal/watch/agent.go`,
  `internal/task/service.go`
- `internal/tools/broker.go`, `budget.go`
- `internal/approval/approval.go`, `direct.go`
- `internal/reaction/reaction.go`, `internal/telegram/service.go`
- Tests that named the moved constants
- `README.md`, `docs/architecture.md`

## Validation

- Before and after the move, the text of every moved builder for the same
  inputs is compared byte for byte.
- `go test ./internal/prompt` pins the system prompt for a turn without
  tools, with web search only, with tools with and without guidance, and
  with memory and jobs, plus the turn lines, unattended framings, and tool
  and act results.
- The usual formatting, module, vet, build, test, and plan checks.

## Risks and open questions

- A connector's guidance could still carry a per-turn value into the system
  prompt; the pin covers the scaffolding, not each connector's guidance.
- The golden files must be regenerated with every intended wording change;
  that is the point, since the diff then shows the change.
