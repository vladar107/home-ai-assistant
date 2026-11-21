# 0171 — Scheduled model turns run beside chat, with a longer limit

Status: `done`

## Context

A scheduled model turn (`turn.Engine.RunScheduled`) takes the principal's main
chat turn slot before it starts, so a message sent in the main view while
Heads-up or another model-turn job runs waits for the run to finish, with the
typing indicator showing. The slot was added because two Pi processes on one
profile could race when refreshing OAuth credentials. Plan 0048 records that
Pi 1.0.0 removed that race and moved background tasks outside the slot, but
left the slot in place for chat and scheduled runs as "a separate decision".
Agent watch checks, weekly reviews, assessments, extraction, background tasks,
and the Dutch phrase turn already run without a slot, and each chat tab has
its own slot, so a principal already runs several Pi processes at once.

Scheduled model turns are bounded by `HOME_AGENT_SCHEDULED_TURN_TIMEOUT`
(3m). Heads-up runs on a high-effort model and usually takes 35–115 seconds.
A morning run timed out when the model went silent for over two minutes in
the middle of the turn, after its tool calls; Pi does not retry a stall once
a tool has run. The run ended without a Heads-up, and so without the Dutch
phrase, which follows only a successful run. Raising the limit while the run
holds the chat slot would also lengthen how long a stuck run blocks chat.

## Goal

- A chat message in the main view is answered while a scheduled model turn
  of the same principal runs, without waiting for it.
- Scheduled model turns get five minutes by default instead of three.

## Decisions

- `RunScheduled` stops taking the turn slot. It keeps its fresh session,
  read-only tools, turn key, and its own time limit.
- The scheduler's bounds stay as they are: one executing run per principal and
  two across the household (plan 0101). They now limit Pi processes and
  subscription use per principal rather than mirror the chat slot. Pi's own
  process limits stay as well: five in total, at most two background, with
  chat first.
- The default of `HOME_AGENT_SCHEDULED_TURN_TIMEOUT` becomes 5m in code, in the
  compose fallback, in the eval world's fallback, and in `.env.example`,
  `deploy/nas.env.example`, `.env`, and `deploy/.env.nas`; its 5s–10m range is
  unchanged.
- One limit for all scheduled model turns, not one for Heads-up or per model
  effort: the jobs that need more time are the high-effort ones anyway, and a
  per-job or per-effort limit adds configuration with little gain.
- Sending the Dutch phrase alone after a failed Heads-up is out of scope.

## Approach

1. Remove the slot acquisition from `RunScheduled` and update its doc comment,
   the slot comment in `turn.Engine`, and the dispatch comment in the
   scheduler that gives "scheduled turns wait behind their chat" as the reason
   for the per-principal bound.
2. Change the integration test that asserts a chat message waits for a
   scheduled run so it asserts the opposite, and rename it: during a running
   scheduled turn, a chat message reaches the runtime and is answered, and the
   scheduled report is still delivered afterwards.
3. Raise the default scheduled turn timeout and its config test, the compose
   and eval fallbacks, and the env files.
4. Update `docs/architecture.md` and `docs/operations.md`, which say scheduled
   turns hold the turn lane, and name the new default.

## Files affected

- `internal/turn/scheduled.go`, `internal/turn/engine.go` (comment)
- `internal/schedule/service.go` (comment)
- `internal/telegram/schedule_integration_test.go`, and
  `internal/telegram/draft_integration_test.go`, whose waiting-turn typing test
  used a scheduled run to hold the main lane and now holds the slot itself
- `internal/config/conversation.go`, `internal/config/conversation_test.go`
- `internal/eval/world.go`
- `deploy/compose.app.yaml`, `deploy/nas.env.example`, `.env.example`
- `.env`, `deploy/.env.nas` (ignored, updated locally)
- `docs/architecture.md`, `docs/operations.md`, `docs/plans/README.md`

## Execution

One chunk, done by the orchestrator: the change is a few lines, but which
callers relied on the slot needs judgment. Jev was not run for a single
orchestrator chunk.

## Validation

- The changed integration test fails before the code change and passes after;
  the config test expects 5m.
- Format, module, vet, build, the full test suite with and without `-race`,
  and `scripts/check-plans`.
- After deploy, "Run now" on a model-turn job, with a chat message sent while
  it runs, gets the chat reply before the run's report.

## Risks and open questions

- A chat turn and a scheduled turn of one principal can now use the
  subscription at the same time; background tasks and tabs already do. A
  job preview also runs a scheduled turn outside the scheduler's bounds, so a
  preview, a scheduled run, and a chat turn may overlap; Pi's process limits
  still hold.
- A scheduled report can arrive in the main view between a message and its
  reply, as task results and watch alerts already can.
- A stuck scheduled turn now holds a scheduler slot and one of Pi's two
  background process slots for up to five minutes instead of three. When two
  are stuck at once, watch checks, assessments, extraction, reviews, and the
  Dutch phrase queue behind them and can run out their own time limit while
  waiting, since that limit starts before the process slot is taken.
- After a tool call, a silent model stream is only logged and waits for the
  turn's deadline, so a dead connection now takes five minutes to fail
  instead of three. The longer limit helps a slow model, not a dead one; a
  separate cutoff for silence after tool calls is out of scope.
