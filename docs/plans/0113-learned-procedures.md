# 0113 — Learned procedures

Status: `done`

## Context

Plan 0078 designs self-learning in three steps. Plan 0080 built the first,
learning an instruction from a 👎. This is the second: know-how from a
request that finally worked is kept, so the next similar request does not
start from scratch. Today the model has instructions and facts, both short
lines always in the prompt, and nothing that holds steps.

## Goal

- After a request that took several tool calls, or recovered from failures,
  the model can offer to keep how it was done as a procedure. The user
  confirms it on a card that shows the steps.
- The prompt lists only procedure titles. The model reads a procedure's
  steps when a request matches it, in chat turns and background tasks alike.
- `/memory` shows procedures in their own section, each with a Delete button.
- The user can turn procedure learning off in `/settings`.

## Decisions

- **Own table, not a memory kind.** A procedure has a title of up to 80
  characters, steps of up to 1,000, and a last-read time, none of which fit
  `memory_entries` (300 characters, no title). Migration `043` adds
  `memory_procedures`: id (8 hex, the memory ID space), principal, title,
  steps, source (`confirmed` or `learned`), created, updated, and last read.
  A principal keeps at most 20; the 21st is refused with a result that
  tells the model to replace one.
- **Two tools.**
  - `procedure_read {id}`: read risk, so tasks and checks get it too. It
    returns the steps and stamps the last-read time.
  - `procedure_save {title, steps, id?}`: write risk, chat turns only. It
    always proposes, with or without an id (an id replaces that procedure).
    A procedure changes later turns, as an instruction does.
- **The card** reads "Keep this procedure: <title>", then the steps, then
  Confirm or Reject. In an exposed conversation (the check memory writes
  use) the card always carries a fixed warning that the steps were written
  after the conversation read outside content and should be checked before
  confirming. The steps are not screened: a procedure is instructions for
  the assistant by design, so the screener would flag nearly every one and
  mark the user suspicious.
- **`procedure_read` results are the user's text.** A tool definition can be
  marked `Confirmed`: its result holds only text the user confirmed, so the
  broker frames a successful result as "text the user confirmed earlier"
  instead of untrusted outside data. Error results keep today's framing.
  Only `procedure_read` is marked.
- **Prompt.** The memory section gains "Procedures you have kept (read one
  with procedure_read before following it)", one line per procedure,
  `- [id] title`. The prompt changes only when a title changes, so caching
  holds.
- **Guidance.** Propose a procedure only when a request needed several tool
  calls or a recovery from failures to succeed, and the same kind of request
  is likely again, or when the user asks to remember how something was done.
  Steps name the tools, the arguments that mattered, the order, and what to
  avoid. Never include private values that belong in facts.
- **Background tasks read, never propose.** Tasks run read-only and cannot
  send cards. A task can use `procedure_read`. Proposing after a task is left
  to the weekly review (plan 0114).
- **`/memory`.** A "Procedures" section after Facts, with numbered titles and
  "Delete N" buttons. Callbacks are `mem:pdel:<id>:<page>`. "Forget
  everything" clears procedures too.
- **`/settings`.** The Memory section gains "Learn procedures", on by
  default (a `learn_procedures` column on `memory_settings`). Off removes
  `procedure_save` and its guidance. Kept procedures stay listed and
  readable until deleted. Memory off removes procedures from the prompt and
  both tools.
- **`/privacy`** says memory includes the procedures the user kept.
- **Metrics.** `home_agent.memory.writes` counts procedures under kind
  `procedure`, with the existing outcomes. `home_agent.memory.procedure_reads`
  counts reads by principal.
- **Decided during implementation.**
  - `procedure_read` and its guidance are offered only while the principal
    keeps at least one procedure, so a user without any pays nothing for it.
    The memory guidance moves to the write guidance, so a read-only turn gets
    the read guidance alone.
  - A confirmed save gets source `confirmed`. `learned` is left for the
    weekly review (plan 0114), whose confirmed changes are learned.
  - A title already kept, compared without case, is refused with the id to
    save over instead. A new entry's or procedure's id is drawn so it matches
    no entry or procedure of the principal.
  - The title is one line; the steps keep their line breaks, with spacing
    collapsed and empty lines dropped. Only titles reach the prompt.
  - Procedure proposals carry no intent check: they are offers by design, so
    the check would warn on nearly every one. In exposed conversations the
    fixed outside-content warning covers them.
  - A replacement keeps when the replaced procedure last changed, and
    Confirm refuses it if the procedure changed or was deleted since the card
    was shown.
  - "Forget everything" clears entries and procedures in one transaction, and
    its confirmation counts them as items.
  - A card proposed before "Learn procedures" was turned off is still kept if
    confirmed afterwards, as with learning from 👎; only memory off refuses it.

## Approach

1. Migration `043`, the store, and the memory service: the table, the
   setting, caps, `Render`'s new section.
2. The two tools, the proposal and its execution on Confirm, the guidance.
3. Telegram: the `/memory` section and delete, the `/settings` toggle,
   `/privacy`.
4. Metrics, README, `docs/architecture.md` (memory).

## Files affected

- a new migration `043`, `internal/store/sqlite/memory.go`
- `internal/memory/memory.go`, `internal/memory/tools.go`, and a new
  `internal/memory/procedures.go`
- `internal/telegram/memory.go`, which also holds the `/privacy` line
- `internal/observability/telemetry.go`
- `internal/tools/broker.go` (the `Confirmed` flag), `internal/prompt/tool.go`
- README, `docs/architecture.md`

## Validation

- Integration tests with the fake Telegram server and a temporary database:
  - `procedure_save` in a chat turn sends a card with the title and steps;
    Confirm stores it, and the next turn's memory lists its title only;
  - `procedure_read` returns the steps in a chat turn and in a task, and
    stamps the last-read time;
  - saving with an id replaces that procedure; a 21st is refused;
  - in an exposed conversation the card warns without screening, and a
    clean one has no warning;
  - `procedure_read`'s result carries the confirmed framing;
  - `/memory` lists and deletes procedures; "Forget everything" clears them;
  - with "Learn procedures" off, `procedure_save` is not offered and
    `procedure_read` still works; with memory off, neither is offered.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- The model's judgment of "likely again" cannot be checked. The reject rate
  shows whether it over-proposes.
- A procedure can go stale when a tool changes. The weekly review proposes
  removing ones unused for 60 days or naming tools that no longer exist.
