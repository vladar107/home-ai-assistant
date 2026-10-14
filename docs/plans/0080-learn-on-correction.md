# 0080 — Learn on correction

Status: `done`

## Context

Plan 0078 designs self-learning in three steps. This is the first: turn a
👎, or an explicit correction, into a lasting instruction when the user
agrees. Today a 👎 queues one reconsider turn that repairs that reply only
(plan 0064), and corrections are fixed in the moment and forgotten.

## Goal

- After a 👎, the reconsider turn repairs the reply. When the mistake comes
  from a preference or rule that would recur, it also proposes one memory
  instruction. The card says it was learned from that 👎.
- When the user corrects the assistant with a lasting preference, the model
  offers to remember it as an instruction. A one-off fix is not a lesson.
- Confirmed lessons show in `/memory` as learned, and can be deleted there
  like any entry.
- The user can turn learning from 👎 off in `/settings`.

## Decisions

- **The `learned` source.** A memory entry written from a reconsider turn's
  proposal gets source `learned`, a new value beside `model` and
  `confirmed`. Migration `026` rebuilds `memory_entries` for the new check
  value. It also adds a `learn_from_feedback` column to `memory_settings`,
  on by default. Plan 0079 takes migration `027`.
- **Only reconsider turns learn.** The run knows it is a reconsider turn: a
  flag set where the reconsider message is queued, beside `UserMessage`. A
  `memory_add` or `memory_replace` of an instruction in that turn is a
  proposal, as every instruction is today. Its summary is prefixed
  "Learned from your 👎 on <date>:", and execution writes source `learned`.
  Facts in a reconsider turn are never learned this way: the turn's message
  is ours.
- **At most one lesson per reconsider turn.** A second instruction proposal
  in the same reconsider turn is refused with a result telling the model to
  pick the one that matters.
- **The reconsider message gains a second step.** After the better answer:
  if the mistake would recur, because of a preference, a habit of the
  user's, or a rule the assistant should follow, propose one instruction.
  Otherwise propose nothing and do not mention memory. With learning off,
  the step is left out, and the reconsider turn proposes no instruction.
- **Explicit corrections are guidance only.** The memory guidance says that
  when the user corrects the assistant and the correction states a lasting
  preference ("use metric", "always 24-hour times"), the model offers to
  keep it as an instruction. A one-off fix ("I meant the 07:42") is not one.
  These entries use the existing sources; nothing marks them as learned.
- **`/memory`** shows learned entries with a "learned" marker. Deleting one
  works as for any entry.
- **`/settings`**: the Memory section gains "Learn from 👎", on by default.
  Turning memory off turns learning off too.
- **Metrics.** `home_agent.memory.lessons` counts lessons by outcome:
  `proposed` when a reconsider turn proposes one, and `written` when one is
  confirmed. The approval metrics already count rejections by tool.

## Approach

1. Migration `026`, the store, and the memory service: the `learned`
   source, the settings column, and the lesson counter.
2. The reconsider flag on the run, set where the reconsider turn is queued,
   and read by the memory tools.
3. The memory tools: the lesson summary prefix, one lesson per turn, the
   `learned` source on execution, and the guidance on corrections.
4. The reconsider message's second step, which depends on the setting.
5. Telegram: the marker in `/memory`, and the setting in `/settings`.
6. README, `docs/architecture.md` (memory and reactions), and `/privacy` if
   it describes memory sources.

## Files affected

- a new migration `026`, `internal/store/sqlite/memory.go`
- `internal/memory/memory.go`, `internal/memory/tools.go`
- `internal/tools/run.go`, `internal/conversation/reaction.go` and where it
  queues the turn
- `internal/telegram/memory.go`, `internal/telegram/settings.go`
- `internal/observability/telemetry.go`
- README, `docs/architecture.md`

## Validation

- Integration tests with the fake Telegram server and a temporary database:
  - a 👎 starts a reconsider turn whose instruction proposal carries the
    "Learned from your 👎" prefix; Confirm writes it with source `learned`;
    `/memory` marks it;
  - a second instruction proposal in the same reconsider turn is refused;
  - with "Learn from 👎" off, the reconsider message has no learning step,
    and an instruction proposal in that turn is refused;
  - an instruction proposed in an ordinary chat turn keeps today's source
    and has no prefix.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- The model's judgment of "would recur" cannot be checked. The reject rate
  for lessons, from `home_agent.memory.lessons` and the approval metrics,
  shows whether it proposes too much.
- Plan 0079 makes ordinary instruction writes direct. Reconsider turns are
  not started by a user message, so lessons still propose there. The two
  plans agree.
