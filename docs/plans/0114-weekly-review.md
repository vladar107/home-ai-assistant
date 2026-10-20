# 0114 — Weekly review

Status: `done`

## Context

Plan 0078 designs self-learning in three steps; plans 0080 and 0113 build
the first two. This is the third: once a week the application gathers the
week's signals and asks the model what is worth learning from them. Today a
👎 repairs one reply, tool failures reach telemetry only, and nothing looks
back across a week or tidies instructions that overlap.

## Goal

- Once a week, if the user turned it on, one "Weekly review" message offers
  at most three changes to memory and at most two backlog items, each
  confirmed or rejected on one card.
- A week with nothing worth proposing sends nothing.
- Nothing changes without Confirm, and every change says it came from the
  review.

## Decisions

- **Inputs are gathered by code, not by tools.** The review turn gets one
  message holding:
  - every 👎 reply of the past 7 days: the user message before it and the
    reply, each cut to 500 characters, and whether it was reconsidered. A
    new history reader finds them in the Pi session file by the reply's
    session and send time (`conversation_reply_messages`);
  - tool outcomes of the past 7 days, as counts per tool and outcome;
  - the current instructions, and procedures with their last-read dates;
  - how many lessons were confirmed, rejected, or expired in 30 days.
- **Tool outcomes are kept as daily counts.** Migration `044` adds
  `tool_outcome_counts`: principal, day, tool, outcome, count. The broker's
  outcome hook adds one per call. No arguments or results are kept. Rows
  older than 30 days are pruned daily.
- **The model answers in JSON, and code makes the cards.** The review turn
  has no tools, like the Trips scan. It answers with up to three `changes`
  (add or replace an instruction, merge instructions, remove an
  instruction, save or remove a procedure) and up to two `backlog` items.
  Code validates each change against the current memory (ids exist, caps
  hold, no duplicates) and drops the invalid ones.
- **Merge is one change.** A merge names the instructions it replaces and
  the new text. Its card shows the old entries and the new one. On Confirm
  it applies only if those entries are unchanged since, like a replace.
- **Cards.** Valid changes and backlog items become one approval bundle,
  sent to the Reports tab under "Weekly review". Approval gains a way to send
  a bundle outside a chat turn, since today only chat delivery sends one.
  Each item is labelled "From your weekly review". Confirmed memory changes
  get source `learned`.
- **Exposure.** Reply texts can hold outside content. As plan 0113 decided
  for procedures, the instruction and procedure texts of the changes are not
  screened: they are instructions by design, so the screener would flag them
  and mark the user suspicious. Instead every change carries a fixed
  outside-content warning when any 👎 reply in the week's inputs came from a
  conversation that read third-party content (its session's `exposed_at` is
  set), and no such warning otherwise. The model's reasons and the backlog
  items, ordinary prose, are screened, and a flagged one carries a warning.
- **Schedule and control.** A built-in job type `memory.review`, per
  principal, Sunday 18:00 in the job's zone (`HOME_AGENT_TIMEZONE` by
  default). The Memory section of `/settings` gains "Weekly review", off by
  default, with "Run now", like Trips. Memory off stops it.
- **Cost.** One model turn per week on the user's own subscription, with
  the chat default model and effort.
- **`/privacy`** lists the tool outcome counts and their 30-day pruning.
- **Metrics.** `home_agent.memory.lessons` gains a `mechanism` label
  (`feedback`, `procedure`, `review`). The review job reports runs and how
  many changes it sent.
- **Decided during implementation.**
  - An offered bundle is marked by its heading, a new `approvals.heading`
    column in migration `044`. It skips the intent check, since nobody asked
    for it; Confirm continues no conversation and leaves no notice for the
    tab's next turn; and the model cannot withdraw it. It stays confirmable for
    24 hours instead of the usual 10 minutes, and cards show such lifetimes in
    whole hours. Its records are stored in one transaction, and a chat turn's
    proposal never joins it. Like any newer bundle, a chat proposal in the
    Reports tab replaces a pending review card; Review now offers it again.
  - Merging and removing a procedure are approval actions only,
    `memory_merge` and `procedure_remove`, never tools the model is offered. A
    merge gives the first instruction the new text and forgets the others, in
    one store transaction.
  - Each change is checked against memory as the changes before it leave it,
    so together they hold the caps and no entry is touched twice. The first
    three valid changes and the first two valid backlog items are kept, and an
    item too long to show whole on the card is dropped on its own. A change
    may not name an id the review gave what an earlier change adds.
  - The model gives each change a one-sentence reason, shown after the label.
    A flagged reason or backlog item gets a warning of its own, since the
    screener's memory warning is worded for facts.
  - A procedure replacement or removal from the review stores when the
    procedure last changed, and an instruction replacement stores the text the
    card showed, so Confirm refuses one that changed since; a chat turn's
    instruction replacement keeps today's behaviour.
  - A week with no 👎 reply, no failed tool call, fewer than two instructions,
    and no procedure asks no model. The inputs hold the 20 newest 👎 replies,
    and each session's files are read once for all of its replies.
  - The reply must be exactly the JSON asked for, unknown fields included; any
    other reply proposes nothing, and is logged and counted as failed rather
    than reported to the user.
  - The inputs include each procedure's steps, so the model can judge them.
    The turn takes no turn slot, like a watch check, and starts with the time
    line, like every turn.
  - Turning the review on does not run it at once; Review now does. A run the
    user starts ends with a report of what it sent, or that nothing was worth
    proposing; a scheduled one sends only the cards.
  - Tool outcome counts are kept for every user, whether or not the review is
    on, by UTC day and on the wall clock, as the replies and reactions beside
    them are. A call hands its outcome to one writer goroutine through a
    buffered channel, so a busy database never holds the call up; when the
    buffer is full the outcome is dropped and the next write logs how many
    were. The writer is a lifecycle step that writes what is waiting on
    shutdown, and it prunes the counts on the first outcome of each day,
    instead of a job of its own.
  - Procedures the review saves get source `learned`. A chat turn's
    procedure proposals and confirmations now count as lessons with mechanism
    `procedure`. `home_agent.memory.reviews` counts reviews by outcome, and
    `home_agent.memory.review_changes` the proposals sent.
  - `/privacy` names the tool outcome counts in the memory line.

## Approach

1. Migration `044`, the outcome counts, the broker hook, and pruning.
2. The history reader of a reply and the message before it.
3. Approval: sending a bundle outside a chat turn, and the merge action.
4. The review job: gathering, the prompt in `internal/prompt`, parsing,
   validation, cards, and the quiet week.
5. Telegram: the `/settings` toggle and Run now, `/privacy`.
6. Metrics, README, `docs/architecture.md`, `docs/operations.md`.

## Files affected

- a new migration `044`, a new `internal/store/sqlite/tool_outcomes.go`,
  `internal/store/sqlite/approvals.go`, `conversation_history.go`, and
  `memory.go`
- `internal/tools/broker.go` (the outcome hook), `internal/wiring/wiring.go`
  and a new `internal/wiring/review.go`
- `internal/conversation/history/history.go`
- `internal/approval/approval.go`, `withdraw.go`, and a new `offer.go`
- `internal/memory/memory.go`, `tools.go`, `procedures.go`, and new
  `merge.go` and `review.go`
- `internal/prompt/unattended.go` and its golden file
- `internal/turn/scheduled.go` (the review turn)
- `internal/backlog/tools.go` (the review's backlog proposals)
- `internal/schedule/review.go` (the built-in job)
- `internal/telegram/memory.go`, which also holds the `/privacy` line, and
  `internal/app/app.go`
- `internal/observability/telemetry.go`
- README, `docs/architecture.md`, `docs/operations.md`

## Validation

- Integration tests with the fake Telegram server, a fake Pi session file,
  and a temporary database:
  - a run with a 👎 reply and failed tool calls sends the review turn a
    message holding both, and the model's JSON becomes one card in the
    Reports tab; Confirm writes learned entries;
  - an invalid change (unknown id, over the cap, duplicate) is dropped;
  - a merge applies on Confirm, and does nothing when an entry changed since;
  - a week with no proposals sends nothing;
  - a flagged reason or backlog item warns, and change texts are never
    screened; a run whose 👎 reply is from an exposed conversation carries
    the outside-content warning, and a clean one does not;
  - the toggle off stops the job; memory off stops it too.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- **Reading Pi session files** ties the review to Pi's format. Accepted, as
  in plan 0078: the reader sits beside the history parser that already
  depends on it.
- **Proposal fatigue.** At most three changes and two backlog items a week,
  and the reject rate by mechanism shows whether it is noise.
- **A reply's text is matched by time** in its session file, since Pi files
  carry no request IDs. A miss leaves that reply out rather than guessing.
