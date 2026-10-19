# 0111 — A turn engine apart from the conversation coordinator

Status: `in-progress`

## Context

An architecture audit found that `internal/conversation` is two things in one
package: the coordinator of chat lanes (queue, workers, drafts, delivery,
sessions, per-lane model choice) and the engine that runs one model turn
(connection gate, tools, web search switch, memory, jobs, place lines, the
search check, error mapping). Everything unattended reaches the engine by
importing the coordinator: the scheduler, the watches, the background tasks,
the trip scan, and the reactions take `conversation.ScheduledReply`,
`conversation.TaskRun` and friends, so those packages depend on the whole chat
service for a few types and one call. The services are also wired by setters
(`SetRunner`, `SetRewriter`, `SetTravel`, `SetFeeds`, `SetTabs`,
`SetTabReplies`, ...) because the engine is only built together with the
coordinator.

## Goal

`internal/turn` holds the engine: one struct with no goroutines, no Telegram,
and no store. The coordinator, the scheduler, the watches, the tasks, and the
trips use it for runs; none of the unattended packages imports
`internal/conversation` for a run. Everything sent to the model stays
byte-identical, and exposure, approval, delivery, and per-lane model choice
behave as before.

## Decisions

- The engine owns: the connection gate, the tool provider, the web search
  switch, memory, jobs, the search check, the place lines (location and trip),
  the time and user lines, the counting sink, the per-lane turn slots, the
  error kinds and failure replies, and the unattended runs (`RunScheduled`,
  `RunCheck`, `RunTask`, `RewriteAlert`, `NameTab`).
- Chat uses the engine in phases, because the tool hint and the attachment
  read sit between collecting the tools and running: `Setup` (gate, tools, web
  switch, memory), `Compose` (the turn's message from the parts the
  coordinator owns), `Run`, and the lane slots `TryAcquire` and `Acquire`.
- `RunScheduled` keeps taking the principal's main-lane slot; the slot map
  moves into the engine, which is what lets the scheduler stop importing the
  coordinator. `RunCheck`, `RunTask`, `RewriteAlert`, and `NameTab` take no
  slot, as before.
- `takeNotice` and `markTurnExternal` stay in the coordinator: they key
  exposure by the chat turn and mark the run before the search check. The
  engine receives the already marked `*tools.Run` and the notice text as data.
- Lane choice storage, epochs, and `LaneTurn` stay in the coordinator; the
  coordinator resolves the effective model and effort and passes the pair into
  the engine's run. A task passes its stored pair; unattended runs use the
  chat default.
- Error kind strings, `NothingToAdd` (already in `internal/prompt`), and
  `UnavailableReply` keep their values; `conversation` re-exports the replies
  and kinds the delivery code and `telegram` read.
- The value types the engine needs from the scheduler and the trips are small
  leaf values over the repository (`schedule.PromptSection`, `trip.Travel`,
  `trip.Reader`), so the engine can be built before the services that use it.
- The module lifecycle part of the same audit finding was done by plan 0109
  and is not repeated here.

## Approach

1. M1, leaf values: `schedule.PromptSection`, `trip.Travel`, `trip.Reader`;
   telemetry, exposure, safety checks, travel (and the task proposer) become
   config fields of the scheduler, watches, and tasks, with the setters left
   as thin wrappers for the tests.
2. M2, create `internal/turn` by moving the engine code out of
   `internal/conversation`; the coordinator builds an engine inside
   `NewService` and delegates `RunScheduled`, `RunCheck`, `RunTask`,
   `RewriteAlert`, and `NameTab` to it, with temporary aliases for the moved
   types. `runTurn` is rewritten over `Setup`, `Compose`, `Run`, and the slots.
3. M3, point the consumers at the engine: the scheduler's `TurnRunner`, the
   watches' `CheckRunner` and `Rewriter`, the tasks' `Runner`, the trip scan
   adapter, and the reactions take `turn.*` types; the aliases go.
   `conversation.Config` gains `Engine`, `NewService` loses the runtime, tool
   provider, and gate, and the composition root and the Telegram harness build
   the engine first.
4. M4, `telegram.Tabs`: the feed and trip tab resolvers, tab naming, and the
   tab registry maps become one value built before the services that deliver
   into tabs; `SetFeeds`, `SetTabs`, and `SetTabReplies` disappear.
5. M5, the remaining constructor arguments: `trip.Config.Tabs`,
   `task.Config.Runner` (required), `watch.Config.Rewriter`; delete
   `SetTabs`, `SetRunner`, `SetRewriter`, `SetTravel`, and their "call before
   Start" comments.
6. M6, module lifecycle: done by plan 0109 before this plan; skipped.
7. M7, docs: `docs/architecture.md` gains the turn engine and its edges and
   drops the scheduler, task, and watch edges to the coordinator.

## Files affected

- `internal/turn` (new), `internal/conversation`, `internal/schedule`,
  `internal/watch`, `internal/task`, `internal/trip`, `internal/reaction`,
  `internal/wiring`, `internal/telegram` (harness and tabs), and
  `docs/architecture.md`.

## Validation

- The prompt golden files and the Telegram integration tests, which capture
  the exact message and system prompt the runtime receives, are the oracle
  for byte-identical model input; the approval, search check, delivery,
  cancel, card, feed, and trip tab tests cover exposure and delivery.
- Formatting, module, vet, build, race tests, and the plan check.

## Risks and open questions

- `runTurn` carries ordering that is easy to break: the gate before the tools,
  the hint start before the web switch and memory, attachments before the hint
  wait, the exchange record after setup errors, the notice taken before the
  run so the exposure mark precedes the search check, memory notes taken after
  the run whether or not it failed, and turn metrics only when the turn was not
  cancelled.
- Deviations from the design are recorded below as the steps land.

## Deviations

- M1 did not need the leaf values for construction order, since the engine can
  be built where the coordinator is built today; they are still used there so
  the engine does not hold the scheduler or the trip service. The scheduler
  and watch `Feeds`, the watch `Rewriter`, and the task `Runner` config fields
  wait for M4 and M5, where their producers exist.
